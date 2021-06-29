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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/httputil"
	"github.com/b97tsk/log"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	AppIDList []string `yaml:"appids"`

	Proxy chrome.Proxy `yaml:"over"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions
}

type Service struct{}

const ServiceName = "goagent"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(ServiceName)

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

	listener := newListener(ctx.Context)
	defer listener.Stop()

	handler := newHandler(ctx, listener, optsOut)
	defer handler.CloseIdleConnections()

	var (
		server         *http.Server
		serverDown     chan struct{}
		serverListener net.Listener
	)

	startServer := func() error {
		if server != nil {
			return nil
		}

		opts := <-optsOut

		ln, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = &http.Server{
			Handler:      handler,
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
			ErrorLog:     logger.Get(log.LevelDebug),
		}
		serverDown = make(chan struct{})
		serverListener = ln

		go func() {
			_ = server.Serve(listener)

			close(serverDown)
		}()

		go func() {
			var tempDelay time.Duration // how long to sleep on accept failure

			for {
				conn, err := ln.Accept()
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Temporary() {
						if tempDelay == 0 {
							tempDelay = 5 * time.Millisecond
						} else {
							tempDelay *= 2
						}

						if max := 1 * time.Second; tempDelay > max {
							tempDelay = max
						}

						time.Sleep(tempDelay)

						continue
					}

					return
				}

				tempDelay = 0

				go listener.Inject(conn)
			}
		}()

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Infof("stopped listening on %v", serverListener.Addr())

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_ = serverListener.Close() // Manually closes it since we didn't pass it to server.Serve.
		_ = server.Shutdown(ctx)

		server = nil
		serverDown = nil
		serverListener = nil
	}
	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case <-serverDown:
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if len(new.AppIDList) == 0 {
					new.AppIDList = defaultAppIDList
				}

				if !isTwoAppIDListsIdentical(new.AppIDList, old.AppIDList) {
					handler.SetAppIDList(new.AppIDList)
				}

				optsIn <- new
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
			}
		}
	}
}

type listener struct {
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
	lane   chan net.Conn
}

func newListener(ctx context.Context) *listener {
	ctx, cancel := context.WithCancel(ctx)

	return &listener{
		ctx:    ctx,
		cancel: cancel,
		lane:   make(chan net.Conn, 32),
	}
}

func (l *listener) Stop() {
	l.cancel()

	l.mu.Lock()
	lane := l.lane
	l.lane = nil
	l.mu.Unlock()

	if lane == nil {
		return
	}

	close(lane)

	for conn := range lane {
		conn.Close()
	}
}

func (l *listener) Inject(conn net.Conn) { //nolint:interfacer
	l.mu.RLock()

	if l.lane != nil {
		select {
		case <-l.ctx.Done():
			conn.Close()
		case l.lane <- conn:
		}
	} else {
		conn.Close()
	}

	l.mu.RUnlock()
}

func (l *listener) Accept() (net.Conn, error) {
	l.mu.RLock()
	lane := l.lane //nolint:ifshort
	l.mu.RUnlock()

	if lane != nil {
		select {
		case <-l.ctx.Done():
		case conn := <-lane:
			if conn != nil {
				return conn, nil
			}
		}
	}

	return nil, errors.New("service stopped")
}

func (l *listener) Close() error {
	return nil
}

func (l *listener) Addr() net.Addr {
	return nil
}

type handler struct {
	ctx          chrome.Context
	ln           *listener
	opts         <-chan Options
	tr           *http.Transport
	appIDMutex   sync.Mutex
	appIDList    []string
	badAppIDList []string
}

func newHandler(ctx chrome.Context, ln *listener, opts <-chan Options) *handler {
	h := &handler{ctx: ctx, ln: ln, opts: opts}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opts := <-h.opts

		return h.ctx.Manager.Dial(ctx, opts.Proxy.Dialer(), network, addr, opts.Dial.Timeout)
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

func (h *handler) CloseIdleConnections() {
	h.tr.CloseIdleConnections()
}

func (h *handler) SetAppIDList(appIDList []string) {
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

func (h *handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		h.handleConnect(rw, req)
		return
	}

	outreq := req.Clone(req.Context())
	outreq.Close = false
	outreq.RequestURI = ""

	if outreq.URL.Scheme == "" {
		outreq.URL.Scheme = "http"
	}

	if outreq.URL.Host == "" {
		outreq.URL.Host = outreq.Host
	}

	httputil.RemoveHopbyhopHeaders(outreq.Header)

	resp, err := h.roundTrip(outreq)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()

	h.autoRange(outreq, resp)

	if r := outreq.Header.Get("Range"); r != "" {
		h.ctx.Manager.Logger(ServiceName).Debugf(
			"(%v) %v %v %v: %v",
			appIDFromRequest(resp.Request),
			outreq.Method, outreq.URL, r,
			resp.Status,
		)
	} else {
		h.ctx.Manager.Logger(ServiceName).Debugf(
			"(%v) %v %v: %v",
			appIDFromRequest(resp.Request),
			outreq.Method, outreq.URL,
			resp.Status,
		)
	}

	httputil.RemoveHopbyhopHeaders(resp.Header)

	header := rw.Header()
	for key, values := range resp.Header {
		header[key] = values
	}

	if _, ok := header["Content-Length"]; !ok && resp.ContentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
}

func (h *handler) hijack(rw http.ResponseWriter, handle func(net.Conn)) {
	if _, ok := rw.(http.Hijacker); !ok {
		h.ctx.Manager.Logger(ServiceName).Debug("hijack: impossible")
		panic(http.ErrAbortHandler)
	}

	conn, _, err := rw.(http.Hijacker).Hijack()
	if err != nil {
		h.ctx.Manager.Logger(ServiceName).Debugf("hijack: %v", err)
		panic(http.ErrAbortHandler)
	}

	go handle(conn)
}

func (h *handler) handleConnect(rw http.ResponseWriter, req *http.Request) {
	h.hijack(rw, func(conn net.Conn) {
		const response = "HTTP/1.1 200 OK\r\n\r\n"

		remoteHost := req.RequestURI

		_, port, err := net.SplitHostPort(remoteHost)
		if err == nil && port == "80" { // Transparent proxy only for port 80 right now.
			if _, err := conn.Write([]byte(response)); err != nil {
				h.ctx.Manager.Logger(ServiceName).Tracef("connect: write response to local: %v", err)
				conn.Close()

				return
			}

			h.ln.Inject(conn)

			return
		}

		local, ctx := chrome.NewConnChecker(conn)
		defer local.Close()

		remote, err := h.tr.DialContext(ctx, "tcp", remoteHost)
		if err != nil {
			h.ctx.Manager.Logger(ServiceName).Tracef("connect: dial to remote: %v", err)
			return
		}
		defer remote.Close()

		if _, err := local.Write([]byte(response)); err != nil {
			h.ctx.Manager.Logger(ServiceName).Tracef("connect: write response to local: %v", err)
			return
		}

		opts := <-h.opts

		h.ctx.Manager.Relay(local, remote, opts.Relay)
	})
}

func (h *handler) roundTrip(req *http.Request) (*http.Response, error) {
	numBadAppID := 0
	numBadResponse := 0

	for {
		request, err := h.encodeRequest(req)
		if err != nil {
			h.ctx.Manager.Logger(ServiceName).Debug(err)
			return nil, fmt.Errorf("round trip: %w", err)
		}

		appID := appIDFromRequest(request)

		resp, err := h.tr.RoundTrip(request)
		if err != nil {
			h.ctx.Manager.Logger(ServiceName).Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, err)
			return nil, fmt.Errorf("round trip: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			h.ctx.Manager.Logger(ServiceName).Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, resp.Status)

			resp.Body.Close()

			numBadAppID++
			numBadResponse = 0 //nolint:wsl

			h.putBadAppID(appID)

			if numBadAppID == perRequestBadAppIDLimit {
				return nil, errors.New("round trip: too many bad app ids")
			}

			continue
		}

		response, err := h.decodeResponse(resp)
		if err != nil {
			h.ctx.Manager.Logger(ServiceName).Debugf("(%v) %v %v: %v", appID, req.Method, req.URL, err)

			resp.Body.Close()

			numBadResponse++

			if numBadResponse == perRequestBadResponseLimit {
				numBadAppID++
				numBadResponse = 0 //nolint:wsl

				h.putBadAppID(appID)

				if numBadAppID == perRequestBadAppIDLimit {
					return nil, errors.New("round trip: too many bad app ids")
				}
			}

			continue
		}

		return response, nil
	}
}

func (h *handler) autoRange(req *http.Request, resp *http.Response) (yes bool) {
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

func (h *handler) encodeRequest(req *http.Request) (*http.Request, error) {
	var b bytes.Buffer

	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
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
		return nil, errors.New("encode request: no appid")
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
		Body:          io.NopCloser(&buffers),
	}

	return request.WithContext(req.Context()), nil
}

func (h *handler) decodeResponse(resp *http.Response) (*http.Response, error) {
	var hdrLen uint16
	if err := binary.Read(resp.Body, binary.BigEndian, &hdrLen); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(resp.Body, hdrBuf); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(flate.NewReader(bytes.NewReader(hdrBuf))), resp.Request)
	if err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	body, _ := io.ReadAll(response.Body)
	response.Body.Close()

	response.Body = resp.Body
	if len(body) > 0 {
		response.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(body), resp.Body}
	}

	if cookies := response.Header["Set-Cookie"]; len(cookies) == 1 {
		var parts []string

		for i, c := range strings.Split(cookies[0], ", ") {
			if i == 0 || strings.Contains(strings.Split(c, ";")[0], "=") {
				parts = append(parts, c)
			} else {
				parts[len(parts)-1] += ", " + c
			}
		}

		if len(parts) > 1 {
			response.Header["Set-Cookie"] = parts
		}
	}

	return response, nil
}

func (h *handler) getAppID(req *http.Request) string {
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

func (h *handler) putBadAppID(badAppID string) {
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
	h       *handler
	req     *http.Request
	resp    *http.Response
	reqlist []*autoRangeRequest
	err     error
}

func newAutoRangeBody(
	h *handler, req *http.Request, resp *http.Response,
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
			// Remove finished requests.
			b.reqlist = b.reqlist[i+1:]

			if len(b.reqlist) == 0 {
				b.err = io.EOF
				return
			}

			return n, nil
		}
	}

	b.err = io.EOF

	return 0, b.err
}

func (b *autoRangeBody) Close() error {
	for _, req := range b.reqlist {
		_ = req.Close()
	}

	return b.resp.Body.Close()
}

type autoRangeRequest struct {
	h          *handler
	req        *http.Request
	resp       *http.Response
	rangeFirst int64
	rangeLast  int64

	once sync.Once
	pipe struct {
		*io.PipeReader
		*io.PipeWriter
	}
}

func (r *autoRangeRequest) Init() {
	r.once.Do(r.init)
}

func (r *autoRangeRequest) init() {
	r.pipe.PipeReader, r.pipe.PipeWriter = io.Pipe()

	f := func() error {
		requestSize := r.rangeLast - r.rangeFirst + 1

		readResponse := func(resp *http.Response) error {
			defer resp.Body.Close()

			const bufferSize = 4096
			buf := make([]byte, bufferSize)

			for requestSize > 0 {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					requestSize -= int64(n)

					if _, err := r.pipe.Write(buf[:n]); err != nil {
						return err
					}
				}

				if err != nil {
					r.h.ctx.Manager.Logger(ServiceName).Tracef("read response: %v", err)
					return nil // Retry.
				}
			}

			return io.EOF
		}

		if r.resp != nil { // Handle first response.
			if err := readResponse(r.resp); err != nil {
				return err
			}

			if err := r.req.Context().Err(); err != nil {
				return err
			}
		}

		req := r.req.Clone(r.req.Context())

		for {
			rangeFirst, rangeLast := r.rangeLast-requestSize+1, r.rangeLast
			req.Header.Set("Range", fmt.Sprintf("bytes=%v-%v", rangeFirst, rangeLast))

			resp, err := r.h.roundTrip(req)
			if err != nil {
				return err
			}

			r.h.ctx.Manager.Logger(ServiceName).Tracef(
				"(%v) %v %v %v: %v",
				appIDFromRequest(resp.Request),
				req.Method, req.URL,
				req.Header.Get("Range"),
				resp.Status,
			)

			if resp.StatusCode != http.StatusPartialContent {
				resp.Body.Close()

				return errors.New(resp.Status)
			}

			if err := readResponse(resp); err != nil {
				return err
			}

			if err := req.Context().Err(); err != nil {
				return err
			}
		}
	}

	go func() { _ = r.pipe.PipeWriter.CloseWithError(f()) }()
}

func (r *autoRangeRequest) Read(p []byte) (n int, err error) {
	r.Init()
	return r.pipe.Read(p)
}

func (r *autoRangeRequest) Close() error {
	if r.pipe.PipeReader != nil {
		_ = r.pipe.PipeReader.Close()
	}

	return nil
}

type bytesReadCloser struct {
	Bytes []byte
	io.ReadCloser
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if body, ok := req.Body.(*bytesReadCloser); ok {
		return body.Bytes, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	req.Body = &bytesReadCloser{body, io.NopCloser(bytes.NewReader(body))}

	return body, nil
}

func appIDFromRequest(req *http.Request) string {
	return strings.TrimSuffix(req.Host, ".appspot.com")
}

func shuffleAppIDList(appIDList []string) {
	rand.Shuffle(len(appIDList), func(i, j int) {
		appIDList[i], appIDList[j] = appIDList[j], appIDList[i]
	})
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
		"Cache-Control":       true,
		"Connection":          true,
		"Proxy-Authorization": true,
		"Proxy-Connection":    true,
		"Upgrade":             true,
		"Vary":                true,
		"Via":                 true,
		"X-Chrome-Variations": true,
		"X-Forwarded-For":     true,
	}

	defaultAppIDList = []string{
		"eeffefef", "profound-saga-402", "otod3r", "teefede", "teefet",
		"teeffffe", "teefmeef", "teefmeefwd", "tjl379678792", "wzyabcd",
	}
)

const (
	perRequestSize             = 4 << 20
	perRequestBadAppIDLimit    = 3
	perRequestBadResponseLimit = 2
)
