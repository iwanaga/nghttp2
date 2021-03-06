package nghttp2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/bradfitz/http2"
	"github.com/bradfitz/http2/hpack"
	"github.com/tatsuhiro-t/go-nghttp2"
	"golang.org/x/net/spdy"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	serverBin  = buildDir + "/src/nghttpx"
	serverPort = 3009
	testDir    = buildDir + "/integration-tests"
)

func pair(name, value string) hpack.HeaderField {
	return hpack.HeaderField{
		Name:  name,
		Value: value,
	}
}

type serverTester struct {
	args          []string  // command-line arguments
	cmd           *exec.Cmd // test frontend server process, which is test subject
	url           string    // test frontend server URL
	t             *testing.T
	ts            *httptest.Server // backend server
	conn          net.Conn         // connection to frontend server
	h2PrefaceSent bool             // HTTP/2 preface was sent in conn
	nextStreamID  uint32           // next stream ID
	fr            *http2.Framer    // HTTP/2 framer
	spdyFr        *spdy.Framer     // SPDY/3.1 framer
	headerBlkBuf  bytes.Buffer     // buffer to store encoded header block
	enc           *hpack.Encoder   // HTTP/2 HPACK encoder
	header        http.Header      // received header fields
	dec           *hpack.Decoder   // HTTP/2 HPACK decoder
	authority     string           // server's host:port
	frCh          chan http2.Frame // used for incoming HTTP/2 frame
	spdyFrCh      chan spdy.Frame  // used for incoming SPDY frame
	errCh         chan error
}

// newServerTester creates test context for plain TCP frontend
// connection.
func newServerTester(args []string, t *testing.T, handler http.HandlerFunc) *serverTester {
	return newServerTesterInternal(args, t, handler, false, nil)
}

// newServerTester creates test context for TLS frontend connection.
func newServerTesterTLS(args []string, t *testing.T, handler http.HandlerFunc) *serverTester {
	return newServerTesterInternal(args, t, handler, true, nil)
}

// newServerTester creates test context for TLS frontend connection
// with given clientConfig
func newServerTesterTLSConfig(args []string, t *testing.T, handler http.HandlerFunc, clientConfig *tls.Config) *serverTester {
	return newServerTesterInternal(args, t, handler, true, clientConfig)
}

// newServerTesterInternal creates test context.  If frontendTLS is
// true, set up TLS frontend connection.
func newServerTesterInternal(args []string, t *testing.T, handler http.HandlerFunc, frontendTLS bool, clientConfig *tls.Config) *serverTester {
	ts := httptest.NewUnstartedServer(handler)

	backendTLS := false
	for _, k := range args {
		switch k {
		case "--http2-bridge":
			backendTLS = true
		}
	}
	if backendTLS {
		nghttp2.ConfigureServer(ts.Config, &nghttp2.Server{})
		// According to httptest/server.go, we have to set
		// NextProtos separately for ts.TLS.  NextProtos set
		// in nghttp2.ConfigureServer is effectively ignored.
		ts.TLS = new(tls.Config)
		ts.TLS.NextProtos = append(ts.TLS.NextProtos, "h2-14")
		ts.StartTLS()
		args = append(args, "-k")
	} else {
		ts.Start()
	}
	scheme := "http"
	if frontendTLS {
		scheme = "https"
		args = append(args, testDir+"/server.key", testDir+"/server.crt")
	} else {
		args = append(args, "--frontend-no-tls")
	}

	backendURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("Error parsing URL from httptest.Server: %v", err)
	}

	// URL.Host looks like "127.0.0.1:8080", but we want
	// "127.0.0.1,8080"
	b := "-b" + strings.Replace(backendURL.Host, ":", ",", -1)
	args = append(args, fmt.Sprintf("-f127.0.0.1,%v", serverPort), b,
		"--errorlog-file="+testDir+"/log.txt", "-LINFO")

	authority := fmt.Sprintf("127.0.0.1:%v", serverPort)

	st := &serverTester{
		cmd:          exec.Command(serverBin, args...),
		t:            t,
		ts:           ts,
		url:          fmt.Sprintf("%v://%v", scheme, authority),
		nextStreamID: 1,
		authority:    authority,
		frCh:         make(chan http2.Frame),
		spdyFrCh:     make(chan spdy.Frame),
		errCh:        make(chan error),
	}

	if err := st.cmd.Start(); err != nil {
		st.t.Fatalf("Error starting %v: %v", serverBin, err)
	}

	retry := 0
	for {
		var conn net.Conn
		var err error
		if frontendTLS {
			var tlsConfig *tls.Config
			if clientConfig == nil {
				tlsConfig = new(tls.Config)
			} else {
				tlsConfig = clientConfig
			}
			tlsConfig.InsecureSkipVerify = true
			tlsConfig.NextProtos = []string{"h2-14", "spdy/3.1"}
			conn, err = tls.Dial("tcp", authority, tlsConfig)
		} else {
			conn, err = net.Dial("tcp", authority)
		}
		if err != nil {
			retry += 1
			if retry >= 100 {
				st.Close()
				st.t.Fatalf("Error server is not responding too long; server command-line arguments may be invalid")
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if frontendTLS {
			tlsConn := conn.(*tls.Conn)
			cs := tlsConn.ConnectionState()
			if !cs.NegotiatedProtocolIsMutual {
				st.Close()
				st.t.Fatalf("Error negotiated next protocol is not mutual")
			}
		}
		st.conn = conn
		break
	}

	st.fr = http2.NewFramer(st.conn, st.conn)
	spdyFr, err := spdy.NewFramer(st.conn, st.conn)
	if err != nil {
		st.Close()
		st.t.Fatalf("Error spdy.NewFramer: %v", err)
	}
	st.spdyFr = spdyFr
	st.enc = hpack.NewEncoder(&st.headerBlkBuf)
	st.dec = hpack.NewDecoder(4096, func(f hpack.HeaderField) {
		st.header.Add(f.Name, f.Value)
	})

	return st
}

func (st *serverTester) Close() {
	if st.conn != nil {
		st.conn.Close()
	}
	if st.cmd != nil {
		st.cmd.Process.Kill()
		st.cmd.Wait()
	}
	if st.ts != nil {
		st.ts.Close()
	}
}

func (st *serverTester) readFrame() (http2.Frame, error) {
	go func() {
		f, err := st.fr.ReadFrame()
		if err != nil {
			st.errCh <- err
			return
		}
		st.frCh <- f
	}()

	select {
	case f := <-st.frCh:
		return f, nil
	case err := <-st.errCh:
		return nil, err
	case <-time.After(5 * time.Second):
		return nil, errors.New("timeout waiting for frame")
	}
}

func (st *serverTester) readSpdyFrame() (spdy.Frame, error) {
	go func() {
		f, err := st.spdyFr.ReadFrame()
		if err != nil {
			st.errCh <- err
			return
		}
		st.spdyFrCh <- f
	}()

	select {
	case f := <-st.spdyFrCh:
		return f, nil
	case err := <-st.errCh:
		return nil, err
	case <-time.After(2 * time.Second):
		return nil, errors.New("timeout waiting for frame")
	}
}

type requestParam struct {
	name      string              // name for this request to identify the request in log easily
	streamID  uint32              // stream ID, automatically assigned if 0
	method    string              // method, defaults to GET
	scheme    string              // scheme, defaults to http
	authority string              // authority, defaults to backend server address
	path      string              // path, defaults to /
	header    []hpack.HeaderField // additional request header fields
	body      []byte              // request body
}

func (st *serverTester) http1(rp requestParam) (*serverResponse, error) {
	method := "GET"
	if rp.method != "" {
		method = rp.method
	}

	var body io.Reader
	if rp.body != nil {
		body = bytes.NewBuffer(rp.body)
	}
	req, err := http.NewRequest(method, st.url, body)
	if err != nil {
		return nil, err
	}
	for _, h := range rp.header {
		req.Header.Add(h.Name, h.Value)
	}
	req.Header.Add("Test-Case", rp.name)

	if err := req.Write(st.conn); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(st.conn), req)
	if err != nil {
		return nil, err
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	res := &serverResponse{
		status:    resp.StatusCode,
		header:    resp.Header,
		body:      respBody,
		connClose: resp.Close,
	}

	return res, nil
}

func (st *serverTester) spdy(rp requestParam) (*serverResponse, error) {
	res := &serverResponse{}

	var id spdy.StreamId
	if rp.streamID != 0 {
		id = spdy.StreamId(rp.streamID)
		if id >= spdy.StreamId(st.nextStreamID) && id%2 == 1 {
			st.nextStreamID = uint32(id) + 2
		}
	} else {
		id = spdy.StreamId(st.nextStreamID)
		st.nextStreamID += 2
	}

	method := "GET"
	if rp.method != "" {
		method = rp.method
	}

	scheme := "http"
	if rp.scheme != "" {
		scheme = rp.scheme
	}

	host := st.authority
	if rp.authority != "" {
		host = rp.authority
	}

	path := "/"
	if rp.path != "" {
		path = rp.path
	}

	header := make(http.Header)
	header.Add(":method", method)
	header.Add(":scheme", scheme)
	header.Add(":host", host)
	header.Add(":path", path)
	header.Add(":version", "HTTP/1.1")
	header.Add("test-case", rp.name)
	for _, h := range rp.header {
		header.Add(h.Name, h.Value)
	}

	var synStreamFlags spdy.ControlFlags
	if len(rp.body) == 0 {
		synStreamFlags = spdy.ControlFlagFin
	}
	if err := st.spdyFr.WriteFrame(&spdy.SynStreamFrame{
		CFHeader: spdy.ControlFrameHeader{
			Flags: synStreamFlags,
		},
		StreamId: id,
		Headers:  header,
	}); err != nil {
		return nil, err
	}

	if len(rp.body) != 0 {
		if err := st.spdyFr.WriteFrame(&spdy.DataFrame{
			StreamId: id,
			Flags:    spdy.DataFlagFin,
			Data:     rp.body,
		}); err != nil {
			return nil, err
		}
	}

loop:
	for {
		fr, err := st.readSpdyFrame()
		if err != nil {
			return res, err
		}
		switch f := fr.(type) {
		case *spdy.SynReplyFrame:
			if f.StreamId != id {
				break
			}
			res.header = cloneHeader(f.Headers)
			if _, err := fmt.Sscan(res.header.Get(":status"), &res.status); err != nil {
				return res, fmt.Errorf("Error parsing status code: %v", err)
			}
			if f.CFHeader.Flags&spdy.ControlFlagFin != 0 {
				break loop
			}
		case *spdy.DataFrame:
			if f.StreamId != id {
				break
			}
			res.body = append(res.body, f.Data...)
			if f.Flags&spdy.DataFlagFin != 0 {
				break loop
			}
		case *spdy.RstStreamFrame:
			if f.StreamId != id {
				break
			}
			res.spdyRstErrCode = f.Status
			break loop
		case *spdy.GoAwayFrame:
			if f.Status == spdy.GoAwayOK {
				break
			}
			res.spdyGoAwayErrCode = f.Status
			break loop
		}
	}
	return res, nil
}

func (st *serverTester) http2(rp requestParam) (*serverResponse, error) {
	res := &serverResponse{}
	st.headerBlkBuf.Reset()
	st.header = make(http.Header)

	var id uint32
	if rp.streamID != 0 {
		id = rp.streamID
		if id >= st.nextStreamID && id%2 == 1 {
			st.nextStreamID = id + 2
		}
	} else {
		id = st.nextStreamID
		st.nextStreamID += 2
	}

	if !st.h2PrefaceSent {
		st.h2PrefaceSent = true
		fmt.Fprint(st.conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
		if err := st.fr.WriteSettings(); err != nil {
			return nil, err
		}
	}

	method := "GET"
	if rp.method != "" {
		method = rp.method
	}
	_ = st.enc.WriteField(pair(":method", method))

	scheme := "http"
	if rp.scheme != "" {
		scheme = rp.scheme
	}
	_ = st.enc.WriteField(pair(":scheme", scheme))

	authority := st.authority
	if rp.authority != "" {
		authority = rp.authority
	}
	_ = st.enc.WriteField(pair(":authority", authority))

	path := "/"
	if rp.path != "" {
		path = rp.path
	}
	_ = st.enc.WriteField(pair(":path", path))

	_ = st.enc.WriteField(pair("test-case", rp.name))

	for _, h := range rp.header {
		_ = st.enc.WriteField(h)
	}

	err := st.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      id,
		EndStream:     len(rp.body) == 0,
		EndHeaders:    true,
		BlockFragment: st.headerBlkBuf.Bytes(),
	})
	if err != nil {
		return nil, err
	}

	if len(rp.body) != 0 {
		// TODO we assume rp.body fits in 1 frame
		if err := st.fr.WriteData(id, true, rp.body); err != nil {
			return nil, err
		}
	}

loop:
	for {
		fr, err := st.readFrame()
		if err != nil {
			return res, err
		}
		switch f := fr.(type) {
		case *http2.HeadersFrame:
			_, err := st.dec.Write(f.HeaderBlockFragment())
			if err != nil {
				return res, err
			}
			if f.FrameHeader.StreamID != id {
				st.header = make(http.Header)
				break
			}
			res.header = cloneHeader(st.header)
			var status int
			status, err = strconv.Atoi(res.header.Get(":status"))
			if err != nil {
				return res, fmt.Errorf("Error parsing status code: %v", err)
			}
			res.status = status
			if f.StreamEnded() {
				break loop
			}
		case *http2.DataFrame:
			if f.FrameHeader.StreamID != id {
				break
			}
			res.body = append(res.body, f.Data()...)
			if f.StreamEnded() {
				break loop
			}
		case *http2.RSTStreamFrame:
			if f.FrameHeader.StreamID != id {
				break
			}
			res.errCode = f.ErrCode
			break loop
		case *http2.GoAwayFrame:
			if f.ErrCode == http2.ErrCodeNo {
				break
			}
			res.errCode = f.ErrCode
			res.connErr = true
			break loop
		case *http2.SettingsFrame:
			if f.IsAck() {
				break
			}
			if err := st.fr.WriteSettingsAck(); err != nil {
				return res, err
			}
			// TODO handle PUSH_PROMISE as well, since it alters HPACK context
		}
	}
	return res, nil
}

type serverResponse struct {
	status            int                  // HTTP status code
	header            http.Header          // response header fields
	body              []byte               // response body
	errCode           http2.ErrCode        // error code received in HTTP/2 RST_STREAM or GOAWAY
	connErr           bool                 // true if HTTP/2 connection error
	spdyGoAwayErrCode spdy.GoAwayStatus    // status code received in SPDY RST_STREAM
	spdyRstErrCode    spdy.RstStreamStatus // status code received in SPDY GOAWAY
	connClose         bool                 // Conection: close is included in response header in HTTP/1 test
}

func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

func noopHandler(w http.ResponseWriter, r *http.Request) {}
