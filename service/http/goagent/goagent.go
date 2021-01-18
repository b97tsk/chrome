package goagent

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	AppIDList []string           `yaml:"appids"`
	Proxy     service.ProxyChain `yaml:"over"`
	Dial      struct {
		Timeout time.Duration
	}

	dialer proxy.Dialer
}

type Service struct{}

func (Service) Name() string {
	return "goagent"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		ctx.Logger.Error(err)
		return
	}

	ctx.Logger.Infof("listening on %v", ln.Addr())
	defer ctx.Logger.Infof("stopped listening on %v", ln.Addr())

	optsIn, optsOut := make(chan Options), make(chan Options)
	defer close(optsIn)

	go func() {
		var opts Options

		ok := true
		for ok {
			select {
			case opts, ok = <-optsIn:
			case optsOut <- opts:
			}
		}

		close(optsOut)
	}()

	listener := NewListener(ln, ctx.Done())

	handler := NewHandler(ctx, listener, optsOut)
	defer handler.CloseIdleConnections()

	var (
		server     *http.Server
		serverDown chan error
	)

	initialize := func() {
		if server != nil {
			return
		}

		server = &http.Server{
			Handler:      handler,
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
		}
		serverDown = make(chan error, 1)

		go func() {
			serverDown <- server.Serve(listener)
			close(serverDown)
		}()
	}

	defer func() {
		if server != nil {
			_ = server.Shutdown(context.Background())

			<-serverDown
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-serverDown:
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := <-optsOut
				new.dialer = old.dialer

				if !isTwoAppIDListsIdentical(new.AppIDList, old.AppIDList) {
					handler.SetAppIDList(new.AppIDList)
				}

				if !new.Proxy.Equals(old.Proxy) {
					new.dialer, _ = new.Proxy.NewDialer()
				}

				optsIn <- new

				initialize()
			}
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var opts Options
	if err := yaml.UnmarshalStrict(text, &opts); err != nil {
		return nil, err
	}

	return opts, nil
}

type Listener struct {
	mu   sync.Mutex
	once sync.Once
	ln   net.Listener
	done <-chan struct{}
	lane chan net.Conn
}

func NewListener(ln net.Listener, done <-chan struct{}) *Listener {
	return &Listener{ln: ln, done: done}
}

func (l *Listener) init() {
	lane := make(chan net.Conn, 64)

	l.mu.Lock()
	l.lane = lane
	l.mu.Unlock()

	go func() {
		defer func() {
			l.mu.Lock()
			l.lane = nil
			l.mu.Unlock()

			close(lane)

			for conn := range lane {
				conn.Close()
			}
		}()

		for {
			conn, err := l.ln.Accept()
			if err != nil {
				if isTemporary(err) {
					continue
				}

				return
			}
			lane <- conn
		}
	}()
}

func (l *Listener) Inject(conn net.Conn) {
	l.mu.Lock()
	if l.lane != nil {
		l.lane <- conn
	} else {
		conn.Close()
	}
	l.mu.Unlock()
}

func (l *Listener) Accept() (net.Conn, error) {
	l.once.Do(l.init)

	l.mu.Lock()
	lane := l.lane
	l.mu.Unlock()

	if lane != nil {
		select {
		case <-l.done:
		case conn := <-lane:
			if conn != nil {
				return conn, nil
			}
		}
	}

	return nil, errors.New("service stopped")
}

func (l *Listener) Close() error {
	return l.ln.Close()
}

func (l *Listener) Addr() net.Addr {
	return l.ln.Addr()
}

type Handler struct {
	ctx          service.Context
	ln           *Listener
	tr           *http.Transport
	appIDMutex   sync.Mutex
	appIDList    []string
	badAppIDList []string
}

func NewHandler(ctx service.Context, ln *Listener, opts <-chan Options) *Handler {
	h := &Handler{ctx: ctx, ln: ln}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opts := <-opts

		return h.ctx.Manager.Dial(ctx, opts.dialer, network, addr, opts.Dial.Timeout)
	}

	h.tr = &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return h
}

func (h *Handler) CloseIdleConnections() {
	h.tr.CloseIdleConnections()
}

func (h *Handler) SetAppIDList(appIDList []string) {
	h.appIDMutex.Lock()
	defer h.appIDMutex.Unlock()

	var appIDInUsed string
	if len(h.appIDList) > 0 {
		appIDInUsed = h.appIDList[0]
	}

	h.appIDList = append(h.appIDList[:0], appIDList...)
	h.badAppIDList = h.badAppIDList[:0]
	shuffleAppIDList(h.appIDList)

	if appIDInUsed == "" {
		return
	}

	// Keep currently in used appID at the beginning.
	for i, appID := range h.appIDList {
		if appID == appIDInUsed {
			if i != 0 {
				h.appIDList[0], h.appIDList[i] = h.appIDList[i], h.appIDList[0]
			}

			break
		}
	}
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		if _, ok := rw.(http.Hijacker); !ok {
			http.Error(rw, "http.ResponseWriter does not implement http.Hijacker.", http.StatusInternalServerError)
			return
		}

		conn, _, err := rw.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		requestURI := req.RequestURI

		httpVersion := "HTTP/1.0"
		if req.ProtoAtLeast(1, 1) {
			httpVersion = "HTTP/1.1"
		}

		go func() {
			_, port, err := net.SplitHostPort(requestURI)
			if err == nil && port == "80" { // Transparent proxy only for port 80 right now.
				responseString := httpVersion + " 200 OK\r\n\r\n"
				if _, err := conn.Write([]byte(responseString)); err != nil {
					h.ctx.Logger.Debugf("write: %v", err)
					conn.Close()

					return
				}

				h.ln.Inject(conn)

				return
			}

			defer conn.Close()

			local, ctx := service.NewConnChecker(conn)

			remote, err := h.tr.DialContext(ctx, "tcp", requestURI)
			if err != nil {
				responseString := httpVersion + " 503 Service Unavailable\r\n\r\n"
				if _, err := local.Write([]byte(responseString)); err != nil {
					h.ctx.Logger.Debugf("write: %v", err)
				}

				return
			}
			defer remote.Close()

			responseString := httpVersion + " 200 OK\r\n\r\n"
			if _, err := local.Write([]byte(responseString)); err != nil {
				h.ctx.Logger.Debugf("write: %v", err)
				return
			}

			service.Relay(local, remote)
		}()

		return
	}

	// Enable transparent http proxy.
	if req.URL.Scheme == "" {
		if req.TLS != nil && req.ProtoMajor == 1 {
			req.URL.Scheme = "https"
		} else {
			req.URL.Scheme = "http"
		}

		if req.Host == "" && req.TLS != nil {
			req.Host = req.TLS.ServerName
		}

		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
	}

	resp, err := h.roundTrip(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h.autoRange(req, resp)

	appID := appIDFromRequest(resp.Request)
	h.ctx.Logger.Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, resp.Status)

	for key, values := range resp.Header {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}

	if resp.ContentLength >= 0 && resp.Header.Get("Content-Length") == "" {
		rw.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	rw.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(rw, resp.Body); err != nil {
		h.ctx.Logger.Debugf("(%v) RECV %v: %v", appID, req.URL, err)
	}
}

func (h *Handler) roundTrip(req *http.Request) (*http.Response, error) {
	const NRetries = 3
	for i := 0; i < NRetries; i++ {
		request, err := h.encodeRequest(req)
		if err != nil {
			return nil, fmt.Errorf("roundTrip: %w", err)
		}

		appID := appIDFromRequest(request)

		resp, err := h.tr.RoundTrip(request)
		if err != nil {
			h.ctx.Logger.Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, err)

			if i+1 == NRetries || isRequestCanceled(req) {
				return nil, fmt.Errorf("roundTrip: %w", err)
			}

			continue
		}

		if resp.StatusCode != http.StatusOK {
			h.ctx.Logger.Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, resp.Status)

			if i+1 == NRetries || isRequestCanceled(req) {
				return resp, nil
			}

			if resp.StatusCode == http.StatusServiceUnavailable {
				h.putBadAppID(appID)
				resp.Body.Close()

				continue
			}

			switch resp.StatusCode {
			case http.StatusFound, http.StatusNotFound,
				http.StatusMethodNotAllowed, http.StatusBadGateway:
				resp.Body.Close()
				continue
			}

			return resp, nil
		}

		response, err := h.decodeResponse(resp)
		if err != nil {
			resp.Body.Close()

			return nil, fmt.Errorf("roundTrip: %w", err)
		}

		if i+1 == NRetries || isRequestCanceled(req) {
			return response, nil
		}

		if response.StatusCode != http.StatusBadGateway {
			return response, nil
		}

		response.Body.Close()
	}

	panic("should not happen")
}

func (h *Handler) autoRange(req *http.Request, resp *http.Response) (yes bool) {
	if resp.StatusCode != http.StatusPartialContent {
		return
	}

	contentRange := reContentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if contentRange == nil {
		return // Should not happen.
	}

	contentRangeFirst, _ := strconv.ParseInt(contentRange[1], 10, 64)
	contentRangeLast, _ := strconv.ParseInt(contentRange[2], 10, 64)
	contentLength, _ := strconv.ParseInt(contentRange[3], 10, 64)

	requestRangeFirst, requestRangeLast := int64(0), int64(-1)

	requestRange := reRange.FindStringSubmatch(req.Header.Get("Range"))
	if requestRange != nil {
		requestRangeFirst, _ = strconv.ParseInt(requestRange[1], 10, 64)
		if requestRange[1] != "" {
			requestRangeLast, _ = strconv.ParseInt(requestRange[2], 10, 64)
		}
	}

	if requestRangeFirst != contentRangeFirst {
		return // Should not happen.
	}

	if requestRangeLast < 0 {
		requestRangeLast = contentLength - 1
	}

	if requestRangeLast < contentRangeLast {
		return // Should not happen.
	}

	if requestRangeLast == contentRangeLast {
		return // Exactly matched, very well.
	}

	snapshot := *resp
	bodySize := contentRangeLast - contentRangeFirst + 1
	resp.Body = newAutoRangeBody(h, req, &snapshot, bodySize, requestRangeFirst, requestRangeLast)
	resp.ContentLength = requestRangeLast - requestRangeFirst + 1
	resp.Header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))

	if requestRange != nil {
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes %v-%v/%v", requestRangeFirst, requestRangeLast, contentLength))
	} else {
		resp.Header.Del("Content-Range")
		resp.Status = "200 OK"
		resp.StatusCode = http.StatusOK
	}

	return true
}

func (h *Handler) encodeRequest(req *http.Request) (*http.Request, error) {
	var b bytes.Buffer

	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("encodeRequest: %w", err)
	}

	fmt.Fprintf(w, "%v %v HTTP/1.1\r\n", req.Method, req.URL)
	_ = req.Header.WriteSubset(w, reqWriteExcludeHeader)
	w.Close()

	b0 := make([]byte, 2)
	binary.BigEndian.PutUint16(b0, uint16(b.Len()))

	buffers := append(net.Buffers(nil), b0, b.Bytes())

	if req.ContentLength > 0 {
		bytes, err := readRequestBody(req)
		if err != nil {
			return nil, err
		}

		buffers = append(buffers, bytes)
	}

	var length int64
	for _, buf := range buffers {
		length += int64(len(buf))
	}

	appID := h.getAppID(req)
	if appID == "" {
		return nil, errors.New("encodeRequest: no appid")
	}

	fetchserver := &url.URL{
		Scheme: "https",
		Host:   appID + ".appspot.com",
		Path:   "/_gh/",
	}

	request := &http.Request{
		Method: http.MethodPost,
		URL:    fetchserver,
		Host:   fetchserver.Host,
		Header: http.Header{
			"User-Agent": []string(nil),
		},
		ContentLength: length,
		Body:          ioutil.NopCloser(&buffers),
	}

	return request.WithContext(req.Context()), nil
}

func (h *Handler) decodeResponse(resp *http.Response) (*http.Response, error) {
	var hdrLen uint16
	if err := binary.Read(resp.Body, binary.BigEndian, &hdrLen); err != nil {
		return nil, fmt.Errorf("decodeResponse: %w", err)
	}

	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(resp.Body, hdrBuf); err != nil {
		return nil, fmt.Errorf("decodeResponse: %w", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(flate.NewReader(bytes.NewReader(hdrBuf))), resp.Request)
	if err != nil {
		return nil, fmt.Errorf("decodeResponse: %w", err)
	}

	body, _ := ioutil.ReadAll(response.Body)
	response.Body.Close()

	if len(body) > 0 {
		response.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(body), resp.Body}
	} else {
		response.Body = resp.Body
	}

	if cookies, ok := response.Header["Set-Cookie"]; ok && len(cookies) == 1 {
		var parts []string

		for i, c := range strings.Split(cookies[0], ", ") {
			if i == 0 || strings.Contains(strings.Split(c, ";")[0], "=") {
				parts = append(parts, c)
			} else {
				parts[len(parts)-1] += ", " + c
			}
		}

		if len(parts) > 1 {
			response.Header.Del("Set-Cookie")

			for _, c := range parts {
				response.Header.Add("Set-Cookie", c)
			}
		}
	}

	return response, nil
}

func (h *Handler) getAppID(req *http.Request) string {
	h.appIDMutex.Lock()
	defer h.appIDMutex.Unlock()

	if len(h.appIDList) == 0 {
		return ""
	}

	if req.Header.Get("Range") == "" {
		return h.appIDList[0]
	}

	return h.appIDList[rand.Intn(len(h.appIDList))]
}

func (h *Handler) putBadAppID(badAppID string) {
	h.appIDMutex.Lock()
	defer h.appIDMutex.Unlock()

	// Try to remove badAppID from h.appIDList.
	appIDList := h.appIDList[:0]
	for _, appID := range h.appIDList {
		if appID != badAppID {
			appIDList = append(appIDList, appID)
		}
	}

	if len(appIDList) == len(h.appIDList) {
		return // Not found.
	}

	h.appIDList = appIDList
	h.badAppIDList = append(h.badAppIDList, badAppID)

	if len(h.appIDList) == 0 {
		// Swap both lists, start over again.
		h.appIDList, h.badAppIDList = h.badAppIDList, h.appIDList
		shuffleAppIDList(h.appIDList)
	}
}

type autoRangeBody struct {
	h       *Handler
	req     *http.Request
	resp    *http.Response
	reqlist []*autoRangeRequest
	err     error
}

func newAutoRangeBody(
	h *Handler, req *http.Request, resp *http.Response,
	bodySize, rangeFirst, rangeLast int64,
) *autoRangeBody {
	reqlist := []*autoRangeRequest{{
		h:          h,
		req:        req,
		resp:       resp,
		rangeFirst: rangeFirst,
		rangeLast:  rangeFirst + bodySize - 1,
	}}

	rangeFirst += bodySize
	for rangeFirst <= rangeLast {
		size := rangeLast - rangeFirst + 1
		if size > perRequestSize {
			size = perRequestSize
		}

		reqlist = append(reqlist, &autoRangeRequest{
			h:          h,
			req:        req,
			resp:       nil,
			rangeFirst: rangeFirst,
			rangeLast:  rangeFirst + size - 1,
		})
		rangeFirst += size
	}

	return &autoRangeBody{
		h:       h,
		req:     req,
		resp:    resp,
		reqlist: reqlist,
	}
}

func (b *autoRangeBody) Read(p []byte) (n int, err error) {
	if b.err != nil {
		return 0, b.err
	}

	for i, req := range b.reqlist {
		n, err = req.Read(p)
		if err == nil {
			if i+1 < len(b.reqlist) && req.AboutToComplete() {
				// Start next range request in advance.
				b.reqlist[i+1].Init()
			}

			if i > 0 {
				// Remove finished requests.
				b.reqlist = b.reqlist[i:]
			}

			return
		}

		if err != io.EOF {
			b.err = err
			return
		}

		if n > 0 {
			if i+1 == len(b.reqlist) { // Final request?
				b.err = io.EOF
				return
			}

			// No, we have more.
			// Remove finished requests.
			b.reqlist = b.reqlist[i+1:]

			return n, nil
		}
	}

	b.err = io.EOF

	return 0, b.err
}

func (b *autoRangeBody) Close() error {
	return b.resp.Body.Close()
}

type autoRangeRequest struct {
	h          *Handler
	req        *http.Request
	resp       *http.Response
	rangeFirst int64
	rangeLast  int64

	once      sync.Once
	done      chan struct{}
	buffers   chan []byte
	startTime time.Time
	readSize  int64
	buf       *[]byte
	err       atomic.Value
}

func (r *autoRangeRequest) Init() {
	r.once.Do(r.init)
}

func (r *autoRangeRequest) init() {
	r.done = make(chan struct{})
	r.buffers = make(chan []byte, 1)
	r.startTime = time.Now()

	go func() {
		defer close(r.done)

		requestSize := r.rangeLast - r.rangeFirst + 1
		if requestSize > perRequestSize {
			buf := make([]byte, requestSize)
			r.buf = &buf
		} else {
			r.buf = reqBufferPool.Get().(*[]byte)
		}

		buf := (*r.buf)[:requestSize]

		readResponse := func(resp *http.Response) {
			for len(buf) > 0 {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					for b := buf[:n]; b != nil; {
						select {
						case r.buffers <- b:
							b = nil
						case buf := <-r.buffers:
							b = buf[:len(buf)+len(b)]
						}
					}

					buf = buf[n:]
				}

				if err != nil {
					break
				}
			}
		}

		if r.resp != nil { // Handle first response.
			readResponse(r.resp)
			r.resp.Body.Close()

			if len(buf) == 0 {
				r.err.Store(&io.EOF)
				return
			}

			if isRequestCanceled(r.req) {
				r.err.Store(&context.Canceled)
				return
			}
		}

		const NRetries = 3

		req := r.req.Clone(r.req.Context())

		for i := 0; i < NRetries; i++ {
			rangeFirst, rangeLast := r.rangeLast-int64(len(buf))+1, r.rangeLast
			req.Header.Set("Range", fmt.Sprintf("bytes=%v-%v", rangeFirst, rangeLast))

			resp, err := r.h.roundTrip(req)
			if err != nil {
				if i+1 == NRetries || isRequestCanceled(req) {
					r.err.Store(&err)
					return
				}

				continue
			}

			if resp.StatusCode != http.StatusPartialContent {
				resp.Body.Close()

				if i+1 == NRetries || isRequestCanceled(req) {
					err := errors.New(resp.Status)
					r.err.Store(&err)

					return
				}

				continue
			}

			readResponse(resp)
			resp.Body.Close()

			if len(buf) == 0 {
				r.err.Store(&io.EOF)
				return
			}

			if isRequestCanceled(req) {
				r.err.Store(&context.Canceled)
				return
			}

			i = -1 // Start over.
		}

		panic("should not happen")
	}()
}

func (r *autoRangeRequest) Read(p []byte) (n int, err error) {
	r.Init()
	select {
	case <-r.done:
		err = *r.err.Load().(*error)
		if err != io.EOF {
			if r.buf != nil {
				reqBufferPool.Put(r.buf)
				r.buf = nil
			}

			return // Result in an error.
		}

		if len(r.buffers) > 0 {
			buf := <-r.buffers
			n = copy(p, buf)
			r.readSize += int64(n)

			if n < len(buf) {
				r.buffers <- buf[n:]
				return n, nil
			}
		}

		if r.buf != nil {
			reqBufferPool.Put(r.buf)
			r.buf = nil
		}

		return // EOF
	case buf := <-r.buffers:
		n = copy(p, buf)
		r.readSize += int64(n)

		if n < len(buf) {
			for b := buf[n:]; b != nil; {
				select {
				case r.buffers <- b:
					b = nil
				case buf := <-r.buffers:
					b = b[:len(b)+len(buf)]
				}
			}
		}

		return
	}
}

func (r *autoRangeRequest) Progress() float64 {
	return float64(r.readSize) / float64(r.rangeLast-r.rangeFirst+1)
}

func (r *autoRangeRequest) TimeLeft() float64 {
	readTime := time.Since(r.startTime)
	if readTime < time.Second {
		return math.MaxFloat64
	}

	sizeLeft := (r.rangeLast - r.rangeFirst + 1) - r.readSize
	timeLeft := readTime.Seconds() * (float64(sizeLeft) / float64(r.readSize))

	return timeLeft // in seconds
}

func (r *autoRangeRequest) AboutToComplete() bool {
	return r.Progress() > .5 || r.TimeLeft() < 4
}

type bytesReadCloser struct {
	Bytes []byte
	io.ReadCloser
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if body, ok := req.Body.(*bytesReadCloser); ok {
		return body.Bytes, nil
	}

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("readRequestBody: %w", err)
	}

	req.Body = &bytesReadCloser{body, ioutil.NopCloser(bytes.NewReader(body))}

	return body, nil
}

func isRequestCanceled(req *http.Request) bool {
	return req.Context().Err() != nil
}

func appIDFromRequest(req *http.Request) string {
	return strings.TrimSuffix(req.Host, ".appspot.com")
}

func shuffleAppIDList(appIDList []string) {
	rand.Shuffle(len(appIDList), func(i, j int) {
		appIDList[i], appIDList[j] = appIDList[j], appIDList[i]
	})
}

func isTemporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

func isTwoAppIDListsIdentical(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i, v := range a {
		if v != b[i] {
			return false
		}
	}

	return true
}

var (
	reRange        = regexp.MustCompile(`^bytes=(\d+)-(\d*)$`)
	reContentRange = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

	reqWriteExcludeHeader = map[string]bool{
		"Vary":                true,
		"Via":                 true,
		"X-Forwarded-For":     true,
		"Proxy-Authorization": true,
		"Proxy-Connection":    true,
		"Upgrade":             true,
		"X-Chrome-Variations": true,
		"Connection":          true,
		"Cache-Control":       true,
	}

	reqBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, perRequestSize)
			return &buf
		},
	}
)

const perRequestSize = 4 * 1024 * 1024
