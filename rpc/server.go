package rpc

import (
	"errors"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type ResponseKind byte

const (
	_               ResponseKind = iota
	HandlerResponse              // handler session response
	HandlerPush                  // handler session push
	RemoteResponse               // remote request normal response, represent whether rpc call successfully
)

type RpcKind byte

const (
	_       RpcKind = iota
	SysRpc          // sys namespace rpc
	UserRpc         // user namespace rpc
)

// Precompute the reflect type for error.  Can't use error directly
// because Typeof takes an empty interface value.  This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()
var typeOfBytes = reflect.TypeOf(([]byte)(nil))

type methodType struct {
	sync.Mutex // protects counters
	method     reflect.Method
	ArgType    reflect.Type
	ReplyType  reflect.Type
	numCalls   uint
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
	Sid           uint64   // frontend session id
	Args          []byte   // for args
	Kind          RpcKind  // namespace
	next          *Request // for free list in Server
}

// Response is a header written before every RPC return.  It is used internally
// but documented here as an aid to debugging, such as when analyzing
// network traffic.
type Response struct {
	Kind          ResponseKind // rpc response type
	ServiceMethod string       // echoes that of the Request
	Seq           uint64       // echoes that of the request
	Sid           uint64       // frontend session id
	Reply         []byte       // save reply value
	Error         string       // error, if any.
	Route         string       // exists when ResponseType equal RPC_HANDLER_PUSH
	next          *Response    // for free list in Server
}

// Server represents an RPC Server.
type Server struct {
	Kind       RpcKind             // rpc kind, either SysRpc or UserRpc
	mu         sync.RWMutex        // protects the serviceMap
	serviceMap map[string]*service // all service
	reqLock    sync.Mutex          // protects freeReq
	freeReq    *Request
	respLock   sync.Mutex // protects freeResp
	freeResp   *Response
}

// NewServer returns a new Server.
func NewServer(kind RpcKind) *Server {
	return &Server{Kind: kind, serviceMap: make(map[string]*service)}
}

// SysRpcServer is the system namespace rpc instance of *Server.
var SysRpcServer = NewServer(SysRpc)

// UserRpcServer is the user namespace rpc instance of *Server
var UserRpcServer = NewServer(UserRpc)

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
//	- exported method of exported type
//	- two arguments, both of exported type
//	- the second argument is a pointer
//	- one return value, of type error
// It returns an error if the receiver is not an exported type or has
// no suitable methods. It also logs the error using package log.
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
		s := "rpc.Register: no service name for type " + s.typ.String()
		log.Print(s)
		return errors.New(s)
	}
	if !isExported(sname) && !useName {
		s := "rpc.Register: type " + sname + " is not exported"
		log.Print(s)
		return errors.New(s)
	}
	if _, present := server.serviceMap[sname]; present {
		return errors.New("rpc: service already defined: " + sname)
	}
	s.name = sname

	// Install the methods
	s.method = suitableMethods(server.Kind, s.typ, true)

	if len(s.method) == 0 {
		str := ""

		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(server.Kind, reflect.PtrTo(s.typ), false)
		if len(method) != 0 {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type"
		}
		log.Print(str)
		return errors.New(str)
	}
	server.serviceMap[s.name] = s
	return nil
}

// suitableMethods returns suitable Rpc methods of typ, it will report
// error using log if reportErr is true.
func suitableMethods(kind RpcKind, typ reflect.Type, reportErr bool) map[string]*methodType {
	methods := make(map[string]*methodType)
	if kind == SysRpc {
		for m := 0; m < typ.NumMethod(); m++ {
			method := typ.Method(m)
			mtype := method.Type
			mname := method.Name
			// Method must be exported.
			if method.PkgPath != "" {
				continue
			}
			// Method needs three ins: receiver, *args, *reply.
			if mtype.NumIn() != 3 {
				continue
			}
			// First arg need not be a pointer.
			argType := mtype.In(1)
			if !isExportedOrBuiltinType(argType) {
				if reportErr {
					log.Println(mname, "argument type not exported:", argType)
				}
				continue
			} else if argType.Kind() != reflect.Ptr {
				if reportErr {
					log.Println("method", mname, "reply type not a pointer:", argType)
				}
				continue
			}
			// Second arg must be a pointer.
			replyType := mtype.In(2)
			// Reply type must be exported.
			if !isExportedOrBuiltinType(replyType) {
				if reportErr {
					log.Println("method", mname, "reply type not exported:", replyType)
				}
				continue
			}
			// Method needs one out.
			if mtype.NumOut() != 1 {
				if reportErr {
					log.Println("method", mname, "has wrong number of outs:", mtype.NumOut())
				}
				continue
			}
			// The return type of the method must be error.
			if returnType := mtype.Out(0); returnType != typeOfError {
				if reportErr {
					log.Println("method", mname, "returns", returnType.String(), "not error")
				}
				continue
			}
			methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
		}
	} else if kind == UserRpc {
		for m := 0; m < typ.NumMethod(); m++ {
			method := typ.Method(m)
			mtype := method.Type
			mname := method.Name
			// Method must be exported.
			if method.PkgPath != "" {
				continue
			}
			// Method needs more than 1 parameter
			if mtype.NumIn() < 1 {
				continue
			}
			// Method needs two out, ([]byte, error).
			if mtype.NumOut() != 2 {
				if reportErr {
					log.Println("method", mname, "has wrong number of outs:", mtype.NumOut())
				}
				continue
			}
			// The return type of the method must be error.
			if returnType := mtype.Out(0); returnType != typeOfBytes {
				fmt.Println(returnType, typeOfBytes)
				if reportErr {
					log.Println("method", mname, "returns", returnType.String(), "not error")
				}
				continue
			}
			// The return type of the method must be error.
			if returnType := mtype.Out(1); returnType != typeOfError {
				if reportErr {
					log.Println("method", mname, "returns", returnType.String(), "not error")
				}
				continue
			}
			methods[mname] = &methodType{method: method}
		}
	}
	return methods
}

func (m *methodType) NumCalls() (n uint) {
	m.Lock()
	n = m.numCalls
	m.Unlock()
	return n
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

func (server *Server) Call(serviceMethod string, args []reflect.Value) ([]reflect.Value, error) {
	parts := strings.Split(serviceMethod, ".")
	if len(parts) != 2 {
		return nil, errors.New("wrong route string")
	}
	sname, smethod := parts[0], parts[1]
	if s, present := server.serviceMap[sname]; present && s != nil {
		if m, present := s.method[smethod]; present && m != nil {
			args = append([]reflect.Value{s.rcvr}, args...)
			rets := m.method.Func.Call(args)
			return rets, nil
		} else {
			return nil, errors.New("rpc: " + smethod + " do not exists")
		}
	} else {
		return nil, errors.New("rpc: " + sname + " do not exists")
	}
}

var rpcResponseKindNames = []string{
	HandlerResponse: "HandlerResponse",
	HandlerPush:     "HandlerPush",
	RemoteResponse:  "RemoteResponse",
}

func (k ResponseKind) String() string {
	if int(k) < len(rpcResponseKindNames) {
		return rpcResponseKindNames[k]
	}
	return strconv.Itoa(int(k))
}

var rpcKindNames = []string{
	SysRpc:  "SysRpc",  // system rpc
	UserRpc: "UserRpc", // user rpc
}

func (k RpcKind) String() string {
	if int(k) < len(rpcKindNames) {
		return rpcKindNames[k]
	}
	return strconv.Itoa(int(k))
}
