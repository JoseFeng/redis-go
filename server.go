package redis

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dolab/objconv"
	"github.com/dolab/objconv/resp"

	"github.com/dolab/redis-go/metrics"
)

// A ResponseWriter interface is used by a Redis handler to construct an Redis
// response.
//
// A ResponseWriter may not be used after the Handler.ServeRedis method has
// returned.
type ResponseWriter interface {
	// WriteStream is called if the server handler is going to produce a list of
	// values by calling Write repeatedly n times.
	//
	// The method cannot be called more than once, or after Write was called.
	WriteStream(n int) error

	// Write is called by the server handler to send values back to the client.
	//
	// Write may not be called more than once, or more than n times, when n is
	// passed to a previous call to WriteStream.
	Write(v interface{}) error
}

// The Flusher interface is implemented by ResponseWriters that allow a Redis
// handler to flush buffered data to the client.
type Flusher interface {
	// Flush sends any buffered data to the client.
	Flush() error
}

// The Hijacker interface is implemented by ResponseWriters that allow a Redis
// handler to take over the connection.
type Hijacker interface {
	// Hijack lets the caller take over the connection. After a call to Hijack
	// the Redis server library will not do anything else with the connection.
	//
	// It becomes the caller's responsibility to manage and close the
	// connection.
	//
	// The returned net.Conn may have read or write deadlines already set,
	// depending on the configuration of the Server. It is the caller's
	// responsibility to set or clear those deadlines as needed.
	//
	// The returned bufio.Reader may contain unprocessed buffered data from the
	// client.
	Hijack() (net.Conn, *bufio.ReadWriter, error)
}

// A Server defines parameters for running a Redis server.
type Server struct {
	// The address to listen on, ":6379" if empty.
	//
	// The address may be prefixed with "tcp://" or "unix://" to specify the
	// type of network to listen on.
	Addr string

	// Handler invoked to handle Redis requests, must not be nil.
	Handler Handler

	// features of command retry and pipeline
	EnableRetry    bool
	EnablePipeline bool

	// ReadTimeout is the maximum duration for reading the entire request,
	// including the reading the argument list.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out writes of the
	// response. It is reset whenever a new request is read.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the next request.
	// If IdleTimeout is zero, the value of ReadTimeout is used. If both are
	// zero, there is no timeout.
	IdleTimeout time.Duration

	// ErrorLog specifies an optional logger for errors accepting connections
	// and unexpected behavior from handlers. If nil, logging goes to os.Stderr
	// via the log package's standard logger.
	ErrorLog Logger

	ConnState   func(net.Conn, http.ConnState)
	mutex       sync.Mutex
	serveOnce   sync.Once
	listeners   map[net.Listener]struct{}
	connections map[*Conn]struct{}
	context     context.Context
	shutdown    context.CancelFunc
}

func (s *Server) WithMetrics(opts metrics.Options) *Server {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	gometrics = metrics.NewMetrics(opts)

	return s
}

// ListenAndServe listens on the network address s.Addr and then calls Serve to
// handle requests on incoming connections. If s.Addr is blank, ":6379" is used.
// ListenAndServe always returns a non-nil error.
func (s *Server) ListenAndServe() error {
	addr := s.Addr
	if len(addr) == 0 {
		addr = ":6379"
	}

	network, address := splitNetworkAddress(addr)
	if len(network) == 0 {
		network = "tcp"
	}

	l, err := net.Listen(network, address)
	if err != nil {
		return err
	}

	return s.Serve(l)
}

// Close immediately closes all active net.Listeners and any connections.
// For a graceful shutdown, use Shutdown.
func (s *Server) Close() error {
	var err error
	s.mutex.Lock()

	if s.shutdown != nil {
		s.shutdown()
	}

	for l := range s.listeners {
		if cerr := l.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}

	for c := range s.connections {
		c.Close()
	}

	s.mutex.Unlock()
	return err
}

// Shutdown gracefully shuts down the server without interrupting any active
// connections. Shutdown works by first closing all open listeners, then closing
// all idle connections, and then waiting indefinitely for connections to return
// to idle and then shut down. If the provided context expires before the shutdown
// is complete, then the context's error is returned.
func (s *Server) Shutdown(ctx context.Context) error {
	const (
		minPollInterval = 10 * time.Millisecond
		maxPollInterval = 500 * time.Millisecond
	)

	s.mutex.Lock()

	if s.shutdown != nil {
		s.shutdown()
	}

	for l := range s.listeners {
		l.Close()
	}

	s.mutex.Unlock()

	for i := 0; s.numberOfActors() != 0; i++ {
		select {
		case <-ctx.Done():
		case <-time.After(backoff(i, minPollInterval, maxPollInterval)):
		}
	}

	return ctx.Err()
}

// Serve accepts incoming connections on the Listener l, creating a new service
// goroutine for each. The service goroutines read requests and then call
// s.Handler to reply to them.
//
// Serve always returns a non-nil error. After Shutdown or Close, the returned
// error is ErrServerClosed.
func (s *Server) Serve(l net.Listener) error {
	const (
		minBackoffDelay = 10 * time.Millisecond
		maxBackoffDelay = 1000 * time.Millisecond
	)

	defer l.Close()
	defer s.untrackListener(l)

	s.trackListener(l)

	config := serverConfig{
		idleTimeout:  s.IdleTimeout,
		readTimeout:  s.ReadTimeout,
		writeTimeout: s.WriteTimeout,
		retryable:    s.EnableRetry,
	}

	if config.idleTimeout == 0 {
		config.idleTimeout = config.readTimeout
	}

	attempt := 0

	for {
		conn, err := l.Accept()

		if err != nil {
			select {
			default:
			case <-s.context.Done():
				return ErrServerClosed
			}

			switch {
			case isTimeout(err):
				continue
			case isTemporary(err):
				attempt++

				select {
				case <-time.After(backoff(attempt, minBackoffDelay, maxBackoffDelay)):
				case <-s.context.Done():
					return ErrServerClosed
				}

				continue
			default:
				return err
			}
		}

		attempt = 0
		c := NewServerConn(conn, s)
		c.setState(http.StateNew)
		go s.serveConnection(s.context, c, config)
	}
}

func (s *Server) serveConnection(ctx context.Context, c *Conn, config serverConfig) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		c.Close()
		c.setState(http.StateClosed)
	}()

	var (
		remoteAddr = c.RemoteAddr().String()
		localAddr  = c.LocalAddr().String()
	)
	gometrics.IncConnection(remoteAddr, localAddr)
	defer gometrics.DecConnection(remoteAddr, localAddr)

	for {
		select {
		default:
		case <-ctx.Done():
			return
		}

		if c.waitReadyRead(config.idleTimeout) != nil {
			return
		}
		c.setState(http.StateActive)

		c.setTimeout(config.readTimeout)
		cmdReader := c.ReadCommands(config.retryable)

		cmds := make([]Command, 0, 4)
		cmds = append(cmds, Command{})

		if !cmdReader.Read(&cmds[0]) {
			s.log(cmdReader.Close())
			return
		}

		// for transaction
		if cmds[0].Cmd == "MULTI" {
			// Transactions have to be loaded in memory because the server has to
			// interleave responses between each command it receives.
			for {
				lastIndex := len(cmds)
				if lastIndex > 0 {
					cmds[lastIndex-1].loadByteArgs()
				}

				if lastIndex == 0 {
					c.WriteArgs(List("OK")) // response to MULTI
				} else {
					c.WriteArgs(List("QUEUED"))
				}

				cmds = append(cmds, Command{})
				cmd := &cmds[lastIndex]

				if !cmdReader.Read(cmd) {
					cmds = cmds[:lastIndex]
					break
				}
			}

			lastIndex := len(cmds) - 1

			if cmds[lastIndex].Cmd == "DISCARD" {
				cmds[lastIndex].Args.Close()

				if err := c.WriteArgs(List("OK")); err != nil {
					return
				}

				continue // discarded transactions are not passed to the handler
			}

			cmds = cmds[1:lastIndex]
		}

		if err := s.serveCommands(c, remoteAddr, cmds, config); err != nil {
			s.log(err)
			return
		}

		if err := cmdReader.Close(); err != nil {
			s.log(err)
			return
		}
		c.setState(http.StateIdle)
	}
}

func (s *Server) serveCommands(c *Conn, addr string, cmds []Command, config serverConfig) (err error) {
	var (
		names      = make([]string, len(cmds))
		remoteAddr = metrics.TrimPort(addr)
		localAddr  = metrics.TrimPort(c.LocalAddr().String())
		issuedAt   = time.Now()
	)
	for i, cmd := range cmds {
		names[i] = cmd.Cmd
	}

	// inc request and commands of processing
	gometrics.IncRequest(remoteAddr, localAddr)
	gometrics.IncCommands(remoteAddr, localAddr, names)

	ctx, cancel := context.WithTimeout(context.Background(), config.readTimeout)

	req := &Request{
		Addr:    addr,
		Cmds:    cmds,
		Context: ctx,
	}

	res := &responseWriter{
		conn:    c,
		timeout: config.writeTimeout,
	}

	err = s.serveRequest(res, req)

	// is this a pipeline?
	reqErr := req.Close()
	if s.EnablePipeline && err == nil && reqErr == nil {
		pipeErr := s.servePipeline(c, addr, cmds, config)
		if pipeErr != ErrNotPipeline {
			err = pipeErr
		}
	}

	// cancel context
	cancel()

	// for request duration
	gometrics.ObserveRequest(remoteAddr, localAddr, issuedAt)

	// dec request and commands of processing
	gometrics.DecRequest(remoteAddr, localAddr)
	gometrics.DecCommands(remoteAddr, localAddr, names)
	if err != nil {
		gometrics.IncErrors(remoteAddr, localAddr, names)
	}
	return
}

func (s *Server) servePipeline(c *Conn, addr string, cmds []Command, config serverConfig) (err error) {
	var (
		pipeCmds []Command
	)
	for _, cmd := range cmds {
		pipeCmd, pipeErr := cmd.pipeCommand()
		if pipeErr != nil {
			err = pipeErr
			break
		}

		pipeCmds = append(pipeCmds, pipeCmd)
	}

	if err != nil {
		return
	}

	if len(pipeCmds) > 0 {
		// TODO: This is for temporary solution and it should refactor to pipeline way!
		c.setTimeout(config.readTimeout)

		err = s.serveCommands(c, addr, pipeCmds, config)
	}

	return
}

func (s *Server) serveRequest(res *responseWriter, req *Request) (err error) {
	var w ResponseWriter = res
	var (
		preparedRes *preparedResponseWriter
		i           int
	)

	addPreparedResponse := func(i int, v interface{}) {
		if preparedRes == nil {
			preparedRes = &preparedResponseWriter{base: res}
		}
		preparedRes.responses = append(preparedRes.responses, preparedResponse{
			index: i,
			value: v,
		})
	}

	for _, cmd := range req.Cmds {
		switch cmd.Cmd {
		case "PING":
			msg := "PONG"
			cmd.ParseArgs(&msg)
			addPreparedResponse(i, msg)

		default:
			req.Cmds[i] = cmd
			i++
		}
	}

	if preparedRes != nil {
		w = preparedRes

		w.WriteStream(len(req.Cmds) + len(preparedRes.responses))
	}

	if req.Cmds = req.Cmds[:i]; len(req.Cmds) != 0 {
		err = s.serveRedis(w, req)
	}

	if err == nil && preparedRes != nil {
		err = preparedRes.writeRemainingValues()
	}

	if err == nil {
		err = res.Flush()
	}

	return
}

func (s *Server) serveRedis(res ResponseWriter, req *Request) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = convertPanicToError(v)
		}
	}()

	s.Handler.ServeRedis(res, req)
	return
}

func (s *Server) log(err error) {
	if err == ErrHijacked || err == ErrNotPipeline {
		return
	}

	printle := log.Print
	if s.ErrorLog != nil {
		printle = s.ErrorLog.Print
	}
	printle(err)
}

func (s *Server) trackListener(l net.Listener) {
	s.mutex.Lock()

	if s.listeners == nil {
		s.listeners = map[net.Listener]struct{}{}
		s.context, s.shutdown = context.WithCancel(context.Background())
	}

	s.listeners[l] = struct{}{}
	s.mutex.Unlock()
}

func (s *Server) untrackListener(l net.Listener) {
	s.mutex.Lock()
	delete(s.listeners, l)
	s.mutex.Unlock()
}

func (s *Server) trackConnection(c *Conn) {
	s.mutex.Lock()

	if s.connections == nil {
		s.connections = map[*Conn]struct{}{}
	}

	s.connections[c] = struct{}{}

	s.mutex.Unlock()
}

func (s *Server) untrackConnection(c *Conn) {
	s.mutex.Lock()

	delete(s.connections, c)

	s.mutex.Unlock()
}

func (s *Server) numberOfActors() int {
	s.mutex.Lock()
	n := len(s.connections) + len(s.listeners)
	s.mutex.Unlock()
	return n
}

// ListenAndServe listens on the network address addr and then calls Serve with
// handler to handle requests on incoming connections.
//
// ListenAndServe always returns a non-nil error.
func ListenAndServe(addr string, handler Handler) error {
	return (&Server{Addr: addr, Handler: handler}).ListenAndServe()
}

// Serve accepts incoming Redis connections on the listener l, creating a new
// service goroutine for each. The service goroutines read requests and then
// call handler to reply to them.
//
// Serve always returns a non-nil error.
func Serve(l net.Listener, handler Handler) error {
	return (&Server{Handler: handler}).Serve(l)
}

func isTimeout(err error) bool {
	e, ok := err.(timeoutError)
	return ok && e.Timeout()
}

func isTemporary(err error) bool {
	e, ok := err.(temporaryError)
	return ok && e.Temporary()
}

type timeoutError interface {
	Timeout() bool
}

type temporaryError interface {
	Temporary() bool
}

type serverConfig struct {
	idleTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	retryable    bool
}

func backoff(attempt int, minDelay time.Duration, maxDelay time.Duration) time.Duration {
	d := time.Duration(attempt*attempt) * minDelay
	if d > maxDelay {
		d = maxDelay
	}

	return d
}

func deadline(timeout time.Duration) time.Time {
	if timeout == 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

func convertPanicToError(v interface{}) (err error) {
	switch x := v.(type) {
	case error:
		err = x
	default:
		err = fmt.Errorf("recovered from redis handler: %v", x)
	}
	return
}

type responseWriterType int

const (
	notype responseWriterType = iota
	oneshot
	stream
)

type responseWriter struct {
	conn    *Conn
	wtype   responseWriterType
	remain  int
	enc     objconv.Encoder
	stream  objconv.StreamEncoder
	timeout time.Duration
}

func (res *responseWriter) WriteStream(n int) error {
	if res.conn == nil {
		return ErrHijacked
	}

	if n < 0 {
		return ErrNegativeStreamCount
	}

	switch res.wtype {
	case oneshot:
		return ErrWriteStreamCalledAfterWrite
	case stream:
		return ErrWriteStreamCalledTooManyTimes
	}

	res.waitReadyWrite()
	res.wtype = stream
	res.remain = n
	res.stream = *resp.NewStreamEncoder(&res.conn.wbuffer)
	return res.stream.Open(n)
}

func (res *responseWriter) Write(val interface{}) error {
	if res.conn == nil {
		return ErrHijacked
	}

	if res.wtype == notype {
		res.waitReadyWrite()
		res.wtype = oneshot
		res.remain = 1
		res.enc = *resp.NewEncoder(&res.conn.wbuffer)
	}

	if res.remain == 0 {
		return ErrWriteCalledTooManyTimes
	}
	res.remain--

	if res.wtype == oneshot {
		return res.enc.Encode(val)
	}

	return res.stream.Encode(val)
}

func (res *responseWriter) Flush() error {
	if res.conn == nil {
		return ErrHijacked
	}

	if res.wtype == notype {
		if err := res.Write("OK"); err != nil {
			return err
		}
	}

	if res.remain != 0 {
		return ErrWriteCalledNotEnoughTimes
	}

	return res.conn.wbuffer.Flush()
}

func (res *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if res.conn == nil {
		return nil, nil, ErrHijacked
	}
	nc := res.conn.conn
	rw := &bufio.ReadWriter{
		Reader: &res.conn.rbuffer,
		Writer: &res.conn.wbuffer,
	}
	res.conn = nil
	res.conn.setState(http.StateHijacked)
	return nc, rw, nil
}

// TODO: figure out here how to wait for the previous response to flush to
// support pipeline.
func (res *responseWriter) waitReadyWrite() {
	if res.timeout != 0 {
		res.conn.setWriteTimeout(res.timeout)
	}
}

type preparedResponseWriter struct {
	base      ResponseWriter
	index     int
	responses []preparedResponse
}

type preparedResponse struct {
	index int
	value interface{}
}

func (res *preparedResponseWriter) WriteStream(n int) error {
	return ErrWriteStreamCalledTooManyTimes
}

func (res *preparedResponseWriter) Write(v interface{}) error {
	if len(res.responses) != 0 && res.responses[0].index == res.index {
		if err := res.base.Write(res.responses[0].value); err != nil {
			return err
		}
		res.responses = res.responses[1:]
	}

	res.index++
	return res.base.Write(v)
}

func (res *preparedResponseWriter) Flush() (err error) {
	if w, ok := res.base.(Flusher); ok {
		err = w.Flush()
	}
	return
}

func (res *preparedResponseWriter) Hijack() (c net.Conn, rw *bufio.ReadWriter, err error) {
	if w, ok := res.base.(Hijacker); ok {
		c, rw, err = w.Hijack()
	} else {
		err = ErrNotHijackable
	}

	return
}

func (res *preparedResponseWriter) writeRemainingValues() (err error) {
	for _, r := range res.responses {
		if err = res.base.Write(r.value); err != nil {
			break
		}
	}
	res.responses = nil
	return
}
