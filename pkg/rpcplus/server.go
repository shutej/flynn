// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
	Package rpc provides access to the exported methods of an object across a
	network or other I/O connection.  A server registers an object, making it visible
	as a service with the name of the type of the object.  After registration, exported
	methods of the object will be accessible remotely.  A server may register multiple
	objects (services) of different types but it is an error to register multiple
	objects of the same type.

	Only methods that satisfy these criteria will be made available for remote access;
	other methods will be ignored:

		- the method is exported.
		- the method has two arguments, both exported (or builtin) types.
		- the method's second argument is either a pointer, or a rpcplus.Stream.
		- the method has return type error.

	In effect, the method must look schematically like one of these two:

		func (t *T) MethodName(argType T1, replyType *T2) error
		func (t *T) MethodName(argType T1, stream rpcplus.Stream) error

	where T, T1 and T2 can be marshaled by encoding/gob.
	These requirements apply even if a different codec is used.
	(In the future, these requirements may soften for custom codecs.)

	The method's first argument represents the arguments provided by the caller; the
	second argument represents the result parameters to be returned to the caller,
	or the function to call to send results.
	The method's return value, if non-nil, is passed back as a string that the client
	sees as if created by errors.New.  If an error is returned, the reply parameter
	will not be sent back to the client.

	The server may handle requests on a single connection by calling ServeConn.  More
	typically it will create a network listener and call Accept or, for an HTTP
	listener, HandleHTTP and http.Serve.

	A client wishing to use the service establishes a connection and then invokes
	NewClient on the connection.  The convenience function Dial (DialHTTP) performs
	both steps for a raw network connection (an HTTP connection).  The resulting
	Client object has two methods, Call and Go, that specify the service and method to
	call, a pointer containing the arguments, and a pointer to receive the result
	parameters. It also has a StreamGo method, that specifies a reply channel
	to receive the results in the case of streaming RPCs.

	The Call method waits for the remote call to complete while the Go method
	launches the call asynchronously and signals completion using the Call
	structure's Done channel. The StreamGo method is always asynchronous.

	Unless an explicit codec is set up, package encoding/gob is used to
	transport the data.

	Here is a simple example.  A server wishes to export an object of type Arith:

		package server

		type Args struct {
			A, B int
		}

		type Quotient struct {
			Quo, Rem int
		}

		type Arith int

		func (t *Arith) Multiply(args *Args, reply *int) error {
			*reply = args.A * args.B
			return nil
		}

		func (t *Arith) Divide(args *Args, quo *Quotient) error {
			if args.B == 0 {
				return errors.New("divide by zero")
			}
			quo.Quo = args.A / args.B
			quo.Rem = args.A % args.B
			return nil
		}

	The server calls (for HTTP service):

		arith := new(Arith)
		rpc.Register(arith)
		rpc.HandleHTTP()
		l, e := net.Listen("tcp", ":1234")
		if e != nil {
			log.Fatal("listen error:", e)
		}
		go http.Serve(l, nil)

	At this point, clients can see a service "Arith" with methods "Arith.Multiply" and
	"Arith.Divide".  To invoke one, a client first dials the server:

		client, err := rpc.DialHTTP("tcp", serverAddress + ":1234")
		if err != nil {
			log.Fatal("dialing:", err)
		}

	Then it can make a remote call:

		// Synchronous call
		args := &server.Args{7,8}
		var reply int
		err = client.Call("Arith.Multiply", args, &reply)
		if err != nil {
			log.Fatal("arith error:", err)
		}
		fmt.Printf("Arith: %d*%d=%d", args.A, args.B, reply)

	or

		// Asynchronous call
		quotient := new(Quotient)
		divCall := client.Go("Arith.Divide", args, &quotient, nil)
		replyCall := <-divCall.Done	// will be equal to divCall
		// check errors, print, etc.

	A server implementation will often provide a simple, type-safe wrapper for the
	client.
*/
package rpcplus

import (
	"bufio"
	"encoding/gob"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Defaults used by HandleHTTP
	DefaultRPCPath   = "/_goRPC_"
	DefaultDebugPath = "/debug/rpc"
)

// Precompute the reflect type for error.  Can't use error directly
// because Typeof takes an empty interface value.  This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()

// RequestLogEntry: a request/response log entry that includes
// timing information and the full RPC method name
type RequestLogEntry struct {
	RequestId     uint64    `json:"request_id"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Duration      int64     `json:"duration"`
	RequestMethod *string   `json:"request_method"`
}

type methodType struct {
	sync.Mutex  // protects counters
	method      reflect.Method
	ArgType     reflect.Type
	ReplyType   reflect.Type
	ContextType reflect.Type
	stream      bool
	numCalls    uint
}

func (m *methodType) TakesContext() bool {
	return m.ContextType != nil
}

type service struct {
	name   string                 // name of service
	rcvr   reflect.Value          // receiver of methods for the service
	typ    reflect.Type           // type of the receiver
	method map[string]*methodType // registered methods
}

// Request is a header written before every RPC call.  It is used internally
// but documented here as an aid to debugging, such as when analyzing
// network traffic.
type Request struct {
	ServiceMethod string   // format: "Service.Method"
	Seq           uint64   // sequence number chosen by client
	next          *Request // for free list in Server
}

// Response is a header written before every RPC return.  It is used internally
// but documented here as an aid to debugging, such as when analyzing
// network traffic.
type Response struct {
	ServiceMethod string    // echoes that of the Request
	Seq           uint64    // echoes that of the request
	Error         string    // error, if any.
	next          *Response // for free list in Server
}

const lastStreamResponseError = "EOS"

type Logger func(*RequestLogEntry)

// Server represents an RPC Server.
type Server struct {
	mu          sync.Mutex // protects the serviceMap
	serviceMap  map[string]*service
	reqLock     sync.Mutex // protects freeReq
	freeReq     *Request
	respLock    sync.Mutex // protects freeResp
	freeResp    *Response
	contextType reflect.Type
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{serviceMap: make(map[string]*service), contextType: reflect.TypeOf("")}
}

// DefaultServer is the default instance of *Server.
var DefaultServer = NewServer()

// Is this an exported - upper case - name?
func isExported(name string) bool {
	rune, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(rune)
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return isExported(t.Name()) || t.PkgPath() == ""
}

// Register publishes in the server the set of methods of the
// receiver value that satisfy the following conditions:
//	- exported method
//	- two arguments, both pointers to exported structs
//	- one return value, of type error
// It returns an error if the receiver is not an exported type or has no
// suitable methods.
// The client accesses each method using a string of the form "Type.Method",
// where Type is the receiver's concrete type.
func (server *Server) Register(rcvr interface{}) error {
	return server.register(rcvr, "", false)
}

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func (server *Server) RegisterName(name string, rcvr interface{}) error {
	return server.register(rcvr, name, true)
}

func (server *Server) SetContextType(typ reflect.Type) {
	server.contextType = typ
}

type Stream struct {
	Send  chan<- interface{}
	Error chan error
}

var typeOfStream = reflect.TypeOf(Stream{})

// prepareMethod returns a methodType for the provided method or nil
// in case if the method was unsuitable.
func prepareMethod(method reflect.Method) *methodType {
	mtype := method.Type
	mname := method.Name
	var replyType, argType, contextType reflect.Type

	stream := false
	// Method must be exported.
	if method.PkgPath != "" {
		return nil
	}

	switch mtype.NumIn() {
	case 3:
		// normal method
		argType = mtype.In(1)
		replyType = mtype.In(2)
		contextType = nil
	case 4:
		// method that takes a context
		argType = mtype.In(2)
		replyType = mtype.In(3)
		contextType = mtype.In(1)
	default:
		log.Println("method", mname, "of", mtype, "has wrong number of ins:", mtype.NumIn())
		return nil
	}

	// First arg need not be a pointer.
	if !isExportedOrBuiltinType(argType) {
		log.Println(mname, "argument type not exported:", argType)
		return nil
	}

	// the second argument will tell us if it's a streaming call
	// or a regular call
	if replyType == typeOfStream {
		// this is a streaming call
		stream = true
	} else if replyType.Kind() != reflect.Ptr {
		log.Println("method", mname, "reply type not a pointer:", replyType)
		return nil
	}

	// Reply type must be exported.
	if !isExportedOrBuiltinType(replyType) {
		log.Println("method", mname, "reply type not exported:", replyType)
		return nil
	}
	// Method needs one out.
	if mtype.NumOut() != 1 {
		log.Println("method", mname, "has wrong number of outs:", mtype.NumOut())
		return nil
	}
	// The return type of the method must be error.
	if returnType := mtype.Out(0); returnType != typeOfError {
		log.Println("method", mname, "returns", returnType.String(), "not error")
		return nil
	}
	return &methodType{method: method, ArgType: argType, ReplyType: replyType, ContextType: contextType, stream: stream}
}

func (server *Server) register(rcvr interface{}, name string, useName bool) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.serviceMap == nil {
		server.serviceMap = make(map[string]*service)
	}
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if useName {
		sname = name
	}
	if sname == "" {
		log.Fatal("rpc: no service name for type", s.typ.String())
	}
	if !isExported(sname) && !useName {
		s := "rpc Register: type " + sname + " is not exported"
		log.Print(s)
		return errors.New(s)
	}
	if _, present := server.serviceMap[sname]; present {
		return errors.New("rpc: service already defined: " + sname)
	}
	s.name = sname
	s.method = make(map[string]*methodType)

	// Install the methods
	for m := 0; m < s.typ.NumMethod(); m++ {
		method := s.typ.Method(m)
		if mt := prepareMethod(method); mt != nil {
			s.method[method.Name] = mt
		}
	}

	if len(s.method) == 0 {
		s := "rpc Register: type " + sname + " has no exported methods of suitable type"
		log.Print(s)
		return errors.New(s)
	}
	server.serviceMap[s.name] = s
	return nil
}

// A value sent as a placeholder for the server's response value when the server
// receives an invalid request. It is never decoded by the client since the Response
// contains an error when it is used.
var invalidRequest = struct{}{}

func (server *Server) sendResponse(sending *sync.Mutex, req *Request, reply interface{}, codec ServerCodec, errmsg string, last bool) (err error) {
	resp := server.getResponse()
	// Encode the response header
	resp.ServiceMethod = req.ServiceMethod
	if errmsg != "" {
		resp.Error = errmsg
		reply = invalidRequest
	}
	resp.Seq = req.Seq
	sending.Lock()
	codec.WriteResponse(resp, reply, last)
	sending.Unlock()
	server.freeResponse(resp)
	return err
}

func (m *methodType) NumCalls() (n uint) {
	m.Lock()
	n = m.numCalls
	m.Unlock()
	return n
}

var nilRes = []reflect.Value{reflect.Zero(typeOfError)}

type call struct {
	server  *Server
	sending *sync.Mutex
	mtype   *methodType
	req     *Request
	argv    reflect.Value
	replyv  reflect.Value
	codec   ServerCodec
	context reflect.Value
	eof     <-chan struct{}
	stop    <-chan struct{}
	done    chan<- struct{}
}

func (s *service) call(c call) {
	c.mtype.Lock()
	c.mtype.numCalls++
	c.mtype.Unlock()
	function := c.mtype.method.Func
	var returnValues []reflect.Value

	if !c.mtype.stream {

		// Invoke the method, providing a new value for the reply.
		if c.mtype.TakesContext() {
			returnValues = function.Call([]reflect.Value{s.rcvr, c.context, c.argv, c.replyv})
		} else {
			returnValues = function.Call([]reflect.Value{s.rcvr, c.argv, c.replyv})
		}

		// The return value for the method is an error.
		errInter := returnValues[0].Interface()
		errmsg := ""
		if errInter != nil {
			errmsg = errInter.(error).Error()
		}
		c.server.sendResponse(c.sending, c.req, c.replyv.Interface(), c.codec, errmsg, true)
		c.server.freeRequest(c.req)
		close(c.done)
		return
	}

	sendChan := make(chan interface{})
	errChan := make(chan error)
	stream := reflect.ValueOf(Stream{sendChan, errChan})
	funcDone := make(chan struct{})
	sendDone := make(chan struct{})

	var streamErr error
	go func() {
		defer close(sendDone)
		for {
			select {
			case data := <-sendChan:
				streamErr = c.server.sendResponse(c.sending, c.req, data, c.codec, "", false)
				if streamErr != nil {
					errChan <- streamErr
					return
				}
			case <-funcDone:
				return
			case <-c.eof:
				errChan <- io.EOF
				return
			case <-c.stop:
				errChan <- io.EOF
				return
			}
		}
	}()

	// Invoke the method, providing a new value for the reply.
	if c.mtype.TakesContext() {
		returnValues = function.Call([]reflect.Value{s.rcvr, c.context, c.argv, stream})
	} else {
		returnValues = function.Call([]reflect.Value{s.rcvr, c.argv, stream})
	}
	close(funcDone)
	<-sendDone

	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		// the function returned an error, we use that
		errmsg = errInter.(error).Error()
	} else if streamErr != nil {
		errmsg = streamErr.Error()
	} else {
		// no error, we send the special EOS error
		errmsg = lastStreamResponseError
	}

	// this is the last packet, we don't do anything with
	// the error here (well sendStreamResponse will log it
	// already)
	c.server.sendResponse(c.sending, c.req, nil, c.codec, errmsg, true)
	c.server.freeRequest(c.req)
	close(c.done)
}

type gobServerCodec struct {
	rwc    io.ReadWriteCloser
	dec    *gob.Decoder
	enc    *gob.Encoder
	encBuf *bufio.Writer
}

func (c *gobServerCodec) ReadRequestHeader(r *Request) error {
	return c.dec.Decode(r)
}

func (c *gobServerCodec) ReadRequestBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *gobServerCodec) WriteResponse(r *Response, body interface{}, last bool) (err error) {
	if err = c.enc.Encode(r); err != nil {
		return
	}
	if err = c.enc.Encode(body); err != nil {
		return
	}
	return c.encBuf.Flush()
}

func (c *gobServerCodec) Close() error {
	return c.rwc.Close()
}

// ServeConn runs the server on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
// The caller typically invokes ServeConn in a go statement.
// ServeConn uses the gob wire format (see package gob) on the
// connection.  To use an alternate codec, use ServeCodec.
func (server *Server) ServeConn(conn io.ReadWriteCloser, loggers ...Logger) {
	server.ServeConnWithContext(conn, nil, loggers...)
}

// ServeConnWithContext is like ServeConn but makes it possible to
// pass a connection context to the RPC methods.
func (server *Server) ServeConnWithContext(conn io.ReadWriteCloser, context interface{}, loggers ...Logger) {
	buf := bufio.NewWriter(conn)
	srv := &gobServerCodec{conn, gob.NewDecoder(conn), gob.NewEncoder(buf), buf}
	server.ServeCodecWithContext(srv, context, loggers...)
}

// ServeCodec is like ServeConn but uses the specified codec to
// decode requests and encode responses.
func (server *Server) ServeCodec(codec ServerCodec, loggers ...Logger) {
	server.ServeCodecWithContext(codec, nil, loggers...)
}

// ServeCodecWithContext is like ServeCodec but it makes it possible
// to pass a connection context to the RPC methods.
func (server *Server) ServeCodecWithContext(codec ServerCodec, context interface{}, loggers ...Logger) {
	sending := new(sync.Mutex)
	eof := make(chan struct{})

	stopChans := make(map[uint64]chan struct{})
	var stopChansMtx sync.Mutex

	var contextVal reflect.Value
	if context != nil {
		contextVal = reflect.ValueOf(context)
	} else {
		contextVal = reflect.New(server.contextType)
	}

	requestLogMap := make(map[uint64]*RequestLogEntry)
	var requestLogMapMtx sync.Mutex

	maybeLog := func(seq uint64) {
		requestLogMapMtx.Lock()
		defer requestLogMapMtx.Unlock()
		// Modify the entry.
		entry, ok := requestLogMap[seq]
		if !ok {
			log.Printf("rpc warning: received bad seqnum %v", seq)
			return
		}
		entry.End = time.Now()
		entry.Duration = int64(entry.End.Sub(entry.Start) / time.Millisecond)

		for _, logger := range loggers {
			logger(entry)
		}

		delete(requestLogMap, seq)
	}

	for {
		service, mtype, req, argv, replyv, keepReading, err := server.readRequest(codec)
		if err != nil {
			// an error here means the request was malformed
			// we won't bother to log these requests/responses
			if err == errCloseStream {
				go func(seq uint64) {
					stopChansMtx.Lock()
					stop, ok := stopChans[seq]
					delete(stopChans, seq)
					stopChansMtx.Unlock()
					maybeLog(seq)
					if !ok {
						return
					}
					close(stop)
				}(req.Seq)
				continue
			}
			if !keepReading {
				break
			}
			// send a response if we actually managed to read a header.
			if req != nil {
				server.sendResponse(sending, req, invalidRequest, codec, err.Error(), true)
				server.freeRequest(req)
			}
			continue
		}
		requestLogMapMtx.Lock()
		requestLogMap[req.Seq] = &RequestLogEntry{
			RequestId:     req.Seq,
			Start:         time.Now(),
			RequestMethod: &req.ServiceMethod,
		}
		requestLogMapMtx.Unlock()
		done := make(chan struct{})
		stop := make(chan struct{})
		stopChansMtx.Lock()
		stopChans[req.Seq] = stop
		stopChansMtx.Unlock()

		go func(seq uint64) {
			<-done
			stopChansMtx.Lock()
			delete(stopChans, seq)
			stopChansMtx.Unlock()
			maybeLog(seq)
		}(req.Seq)

		go service.call(call{
			server:  server,
			sending: sending,
			mtype:   mtype,
			req:     req,
			argv:    argv,
			replyv:  replyv,
			codec:   codec,
			context: contextVal,
			eof:     eof,
			done:    done,
			stop:    stop,
		})
	}
	close(eof)
	codec.Close()
}

func (server *Server) getRequest() *Request {
	server.reqLock.Lock()
	req := server.freeReq
	if req == nil {
		req = new(Request)
	} else {
		server.freeReq = req.next
		*req = Request{}
	}
	server.reqLock.Unlock()
	return req
}

func (server *Server) freeRequest(req *Request) {
	server.reqLock.Lock()
	req.next = server.freeReq
	server.freeReq = req
	server.reqLock.Unlock()
}

func (server *Server) getResponse() *Response {
	server.respLock.Lock()
	resp := server.freeResp
	if resp == nil {
		resp = new(Response)
	} else {
		server.freeResp = resp.next
		*resp = Response{}
	}
	server.respLock.Unlock()
	return resp
}

func (server *Server) freeResponse(resp *Response) {
	server.respLock.Lock()
	resp.next = server.freeResp
	server.freeResp = resp
	server.respLock.Unlock()
}

func (server *Server) readRequest(codec ServerCodec) (service *service, mtype *methodType, req *Request, argv, replyv reflect.Value, keepReading bool, err error) {
	service, mtype, req, keepReading, err = server.readRequestHeader(codec)
	if err != nil {
		if !keepReading {
			return
		}
		// discard body
		codec.ReadRequestBody(nil)
		return
	}

	// Decode the argument value.
	argIsValue := false // if true, need to indirect before calling.
	if mtype.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(mtype.ArgType.Elem())
	} else {
		argv = reflect.New(mtype.ArgType)
		argIsValue = true
	}
	// argv guaranteed to be a pointer now.
	if err = codec.ReadRequestBody(argv.Interface()); err != nil {
		return
	}
	if argIsValue {
		argv = argv.Elem()
	}

	if !mtype.stream {
		replyv = reflect.New(mtype.ReplyType.Elem())
	}
	return
}

var errCloseStream = errors.New("rpc: close stream")

func (server *Server) readRequestHeader(codec ServerCodec) (service *service, mtype *methodType, req *Request, keepReading bool, err error) {
	// Grab the request header.
	req = server.getRequest()
	err = codec.ReadRequestHeader(req)
	if err != nil {
		req = nil
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		err = errors.New("rpc: server cannot decode request: " + err.Error())
		return
	}

	// We read the header successfully.  If we see an error now,
	// we can still recover and move on to the next request.
	keepReading = true

	if req.ServiceMethod == "CloseStream" {
		err = errCloseStream
		return
	}

	serviceMethod := strings.Split(req.ServiceMethod, ".")
	if len(serviceMethod) != 2 {
		err = errors.New("rpc: service/method request ill-formed: " + req.ServiceMethod)
		return
	}
	// Look up the request.
	server.mu.Lock()
	service = server.serviceMap[serviceMethod[0]]
	server.mu.Unlock()
	if service == nil {
		err = errors.New("rpc: can't find service " + req.ServiceMethod)
		return
	}
	mtype = service.method[serviceMethod[1]]
	if mtype == nil {
		err = errors.New("rpc: can't find method " + req.ServiceMethod)
	}
	return
}

// Accept accepts connections on the listener and serves requests
// for each incoming connection.  Accept blocks; the caller typically
// invokes it in a go statement.
func (server *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Fatal("rpc.Serve: accept:", err.Error()) // TODO(r): exit?
		}
		go server.ServeConn(conn)
	}
}

// Register publishes the receiver's methods in the DefaultServer.
func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func RegisterName(name string, rcvr interface{}) error {
	return DefaultServer.RegisterName(name, rcvr)
}

// A ServerCodec implements reading of RPC requests and writing of
// RPC responses for the server side of an RPC session.
// The server calls ReadRequestHeader and ReadRequestBody in pairs
// to read requests from the connection, and it calls WriteResponse to
// write a response back.  The server calls Close when finished with the
// connection. ReadRequestBody may be called with a nil
// argument to force the body of the request to be read and discarded.
type ServerCodec interface {
	ReadRequestHeader(*Request) error
	ReadRequestBody(interface{}) error
	WriteResponse(*Response, interface{}, bool) error

	Close() error
}

// ServeConn runs the DefaultServer on a single connection.
// ServeConn blocks, serving the connection until the client hangs up.
// The caller typically invokes ServeConn in a go statement.
// ServeConn uses the gob wire format (see package gob) on the
// connection.  To use an alternate codec, use ServeCodec.
func ServeConn(conn io.ReadWriteCloser) {
	ServeConnWithContext(conn, nil)
}

// ServeConnWithContext is like ServeConn but it allows to pass a
// connection context to the RPC methods.
func ServeConnWithContext(conn io.ReadWriteCloser, context interface{}) {
	DefaultServer.ServeConnWithContext(conn, context)
}

// ServeCodec is like ServeConn but uses the specified codec to
// decode requests and encode responses.
func ServeCodec(codec ServerCodec) {
	ServeCodecWithContext(codec, nil)
}

// ServeCodecWithContext is like ServeCodec but it allows to pass a
// connection context to the RPC methods.
func ServeCodecWithContext(codec ServerCodec, context interface{}) {
	DefaultServer.ServeCodecWithContext(codec, context)
}

// Accept accepts connections on the listener and serves requests
// to DefaultServer for each incoming connection.
// Accept blocks; the caller typically invokes it in a go statement.
func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

// Can connect to RPC service using HTTP CONNECT to rpcPath.
var connected = "200 Connected to Go RPC"

// ServeHTTP implements an http.Handler that answers RPC requests.
func (server *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "405 must CONNECT\n")
		return
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return
	}
	io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	server.ServeConn(conn)
}

// HandleHTTP registers an HTTP handler for RPC messages on rpcPath,
// and a debugging handler on debugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func (server *Server) HandleHTTP(rpcPath, debugPath string) {
	http.Handle(rpcPath, server)
	http.Handle(debugPath, debugHTTP{server})
}

// HandleHTTP registers an HTTP handler for RPC messages to DefaultServer
// on DefaultRPCPath and a debugging handler on DefaultDebugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func HandleHTTP() {
	DefaultServer.HandleHTTP(DefaultRPCPath, DefaultDebugPath)
}
