package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
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

	"gopkg.in/yaml.v2"
)

type goagentOptions struct {
	AppIDList []string  `yaml:"appids"`
	ProxyList ProxyList `yaml:"over"`
}

type goagentService struct{}

func (goagentService) Run(ctx ServiceCtx) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[goagent] %v\n", err)
		return
	}
	log.Printf("[goagent] listening on %v\n", ln.Addr())
	defer log.Printf("[goagent] stopped listening on %v\n", ln.Addr())

	listener := goagentNewListener(ln, ctx.Done)
	handler := goagentNewHandler(listener)
	server := http.Server{
		Handler: handler,
	}

	serverDown := make(chan error, 1)
	defer func() {
		server.Shutdown(context.TODO())
		<-serverDown
	}()

	go func() {
		serverDown <- server.Serve(listener)
		close(serverDown)
	}()

	var options goagentOptions

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(goagentOptions); ok {
				old := options
				options = new
				if !isTwoAppIDListsIdentical(new.AppIDList, old.AppIDList) {
					handler.SetAppIDList(new.AppIDList)
				}
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.Dialer(direct)
					handler.SetDialFunc(d.Dial)
				}
			}
		case err := <-serverDown:
			log.Printf("[goagent] %v\n", err)
			return
		case <-ctx.Done:
			return
		}
	}
}

func (goagentService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options goagentOptions
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func (goagentService) StandardName() string {
	return "goagent"
}

func init() {
	services.Add("goagent", goagentService{})
}

type goagentListener struct {
	mu   sync.Mutex
	once sync.Once
	ln   net.Listener
	done <-chan struct{}
	lane chan net.Conn
}

func goagentNewListener(ln net.Listener, done <-chan struct{}) *goagentListener {
	return &goagentListener{ln: ln, done: done}
}

func (l *goagentListener) init() {
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
			tcpKeepAlive(conn, direct.KeepAlive)
			lane <- conn
		}
	}()
}

func (l *goagentListener) Inject(conn net.Conn) {
	l.mu.Lock()
	if l.lane != nil {
		l.lane <- conn
	} else {
		conn.Close()
	}
	l.mu.Unlock()
}

func (l *goagentListener) Accept() (net.Conn, error) {
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

func (l *goagentListener) Close() error {
	return l.ln.Close()
}

func (l *goagentListener) Addr() net.Addr {
	return l.ln.Addr()
}

type goagentHandler struct {
	l            *goagentListener
	tr           *http.Transport
	dial         atomic.Value
	appIDMutex   sync.Mutex
	appIDList    []string
	badAppIDList []string
}

func goagentNewHandler(l *goagentListener) *goagentHandler {
	h := &goagentHandler{l: l}
	h.dial.Store(direct.Dial)

	dial := func(network, addr string) (net.Conn, error) {
		dial := h.dial.Load().(func(network, addr string) (net.Conn, error))
		return dial(network, addr)
	}

	h.tr = &http.Transport{
		Dial:                  dial,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return h
}

func (h *goagentHandler) SetDialFunc(dial func(network, addr string) (net.Conn, error)) {
	h.dial.Store(dial)
}

func (h *goagentHandler) SetAppIDList(appIDList []string) {
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

	for i, appID := range h.appIDList {
		if appID == appIDInUsed {
			if i != 0 {
				h.appIDList[0], h.appIDList[i] = h.appIDList[i], h.appIDList[0]
			}
			break
		}
	}
}

func (h *goagentHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		if _, ok := rw.(http.Hijacker); !ok {
			http.Error(rw, "http.ResponseWriter does not implement http.Hijacker.", http.StatusInternalServerError)
			return
		}
		if _, ok := rw.(http.Flusher); !ok {
			http.Error(rw, "http.ResponseWriter does not implement http.Flusher.", http.StatusInternalServerError)
			return
		}

		_, port, err := net.SplitHostPort(req.RequestURI)
		if err == nil && port == "80" {
			rw.WriteHeader(http.StatusOK)
			rw.(http.Flusher).Flush()

			conn, _, err := rw.(http.Hijacker).Hijack()
			if err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
			h.l.Inject(conn)
			return
		}

		remote, err := h.tr.Dial("tcp", req.RequestURI)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer remote.Close()

		rw.WriteHeader(http.StatusOK)
		rw.(http.Flusher).Flush()

		conn, _, err := rw.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		if err := relay(remote, conn); err != nil {
			log.Printf("[goagent] relay: %v\n", err)
		}
		return
	}

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

	appID := appIDFromRequest(resp.Request)
	autoRangeStarted := h.autoRange(req, resp)
	log.Printf("[goagent] (%v) %v %v: %v\n", appID, req.Method, req.URL, resp.Status)

	for key, values := range resp.Header {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}

	if resp.ContentLength >= 0 && resp.Header.Get("Content-Length") == "" {
		rw.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	rw.WriteHeader(resp.StatusCode)
	_, err = io.Copy(rw, resp.Body)
	resp.Body.Close()

	if err != nil {
		if autoRangeStarted {
			log.Printf("[goagent] (%v) AUTORANGE %v: %v\n", appID, req.URL, err)
		} else {
			log.Printf("[goagent] (%v) RECV %v: %v\n", appID, req.URL, err)
		}
	}
}

func (h *goagentHandler) roundTrip(req *http.Request) (*http.Response, error) {
	const NRetries = 3
	for i := 0; i < NRetries; i++ {
		request, err := h.encodeRequest(req)
		if err != nil {
			return nil, fmt.Errorf("encodeRequest: %v", err)
		}
		appID := appIDFromRequest(request)

		resp, err := h.tr.RoundTrip(request)
		if err != nil {
			log.Printf("[goagent] (%v) %v %v: %v\n", appID, req.Method, req.URL, err)
			if i+1 == NRetries || isRequestCanceled(req) {
				return nil, err
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if i+1 == NRetries || isRequestCanceled(req) {
				return resp, nil
			}
			if resp.StatusCode == http.StatusServiceUnavailable {
				log.Printf("[goagent] (%v) over quota\n", appID)
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
			return nil, fmt.Errorf("decodeResponse: %v", err)
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

func (h *goagentHandler) autoRange(req *http.Request, resp *http.Response) (yes bool) {
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

	requestRange := reRange.FindStringSubmatch(req.Header.Get("Range"))
	requestRangeFirst, requestRangeLast := int64(0), int64(-1)
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

	shallowCopy := *resp
	resp.Body = goagentNewAutoRangeBody(h, req, &shallowCopy, requestRangeFirst, requestRangeLast)
	resp.ContentLength = requestRangeLast - requestRangeFirst + 1
	resp.Header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	if requestRange != nil {
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes %v-%v/%v", requestRangeFirst, requestRangeLast, contentLength))
	} else {
		resp.Header.Del("Content-Range")
		resp.Status = "200 OK"
		resp.StatusCode = 200
	}
	return true
}

func (h *goagentHandler) encodeRequest(req *http.Request) (*http.Request, error) {
	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "%v %v HTTP/1.1\r\n", req.Method, req.URL)
	req.Header.WriteSubset(w, reqWriteExcludeHeader)
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
		return nil, errors.New("no appid")
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

func (h *goagentHandler) decodeResponse(resp *http.Response) (*http.Response, error) {
	var hdrLen uint16
	if err := binary.Read(resp.Body, binary.BigEndian, &hdrLen); err != nil {
		return nil, err
	}

	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(resp.Body, hdrBuf); err != nil {
		return nil, err
	}

	response, err := http.ReadResponse(bufio.NewReader(flate.NewReader(bytes.NewReader(hdrBuf))), resp.Request)
	if err != nil {
		return nil, err
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

func (h *goagentHandler) getAppID(req *http.Request) string {
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

func (h *goagentHandler) putBadAppID(badAppID string) {
	h.appIDMutex.Lock()
	defer h.appIDMutex.Unlock()

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
		h.appIDList, h.badAppIDList = h.badAppIDList, h.appIDList
		shuffleAppIDList(h.appIDList)
	}
}

type goagentAutoRangeBody struct {
	h          *goagentHandler
	req        *http.Request
	resp       *http.Response
	rangeFirst int64
	rangeLast  int64
	readSize   int64
	err        error
}

func goagentNewAutoRangeBody(h *goagentHandler, req *http.Request, resp *http.Response, rangeFirst, rangeLast int64) *goagentAutoRangeBody {
	return &goagentAutoRangeBody{
		h:          h,
		req:        req,
		resp:       resp,
		rangeFirst: rangeFirst,
		rangeLast:  rangeLast,
	}
}

func (b *goagentAutoRangeBody) Read(p []byte) (n int, err error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err = b.resp.Body.Read(p)
	if n > 0 {
		b.readSize += int64(n)
		b.rangeFirst += int64(n)
	}
	if err == nil {
		return
	}
	if err != io.EOF {
		b.err = err
		return
	}
	appID := appIDFromRequest(b.resp.Request)
	log.Printf("[goagent] (%v) AUTORANGE %v: recv %v bytes\n", appID, b.req.URL, b.readSize)
	if b.rangeFirst > b.rangeLast {
		b.err = io.EOF
		return
	}
	const (
		NRetries = 3
		MaxSize  = 4 * 1024 * 1024
	)
	rangeFirst, rangeLast := b.rangeFirst, b.rangeLast
	if rangeLast-rangeFirst+1 > MaxSize {
		rangeLast = rangeFirst + MaxSize - 1
	}
	oldRange := b.req.Header.Get("Range")
	defer setOrDeleteHeader(b.req.Header, "Range", oldRange)
	b.req.Header.Set("Range", fmt.Sprintf("bytes=%v-%v", rangeFirst, rangeLast))
	for i := 0; i < NRetries; i++ {
		resp, err := b.h.roundTrip(b.req)
		if err != nil {
			if i+1 == NRetries || isRequestCanceled(b.req) {
				b.err = err
				return n, err
			}
			continue
		}
		b.resp.Body.Close()
		b.resp = resp
		b.readSize = 0
		if resp.StatusCode != http.StatusPartialContent {
			if i+1 == NRetries || isRequestCanceled(b.req) {
				b.err = fmt.Errorf("request bytes=%v-%v: %v", rangeFirst, rangeLast, resp.Status)
				return n, b.err
			}
			continue
		}
		return n, nil
	}
	panic("should not happen")
}

func (b *goagentAutoRangeBody) Close() error {
	return b.resp.Body.Close()
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
		return nil, err
	}
	req.Body = &bytesReadCloser{body, ioutil.NopCloser(bytes.NewReader(body))}
	return body, nil
}

func setOrDeleteHeader(header http.Header, key, value string) {
	if value != "" {
		header.Set(key, value)
	} else {
		header.Del(key)
	}
}

func isRequestCanceled(req *http.Request) (yes bool) {
	select {
	case <-req.Context().Done():
		yes = true
	default:
	}
	return
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
)
