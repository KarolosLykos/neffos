package ws

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type (
	Socket interface {
		NetConn() net.Conn
		Request() *http.Request

		ReadText(timeout time.Duration) (body []byte, err error)
		WriteText(body []byte, timeout time.Duration) error
	}

	// Conn interface {
	// 	Socket() Socket
	// 	ID() string
	// 	String() string

	// 	Write(msg Message) bool
	// 	Ask(ctx context.Context, msg Message) (Message, error)

	// 	Connect(ctx context.Context, namespace string) (NSConn, error)
	// 	WaitConnect(ctx context.Context, namespace string) (NSConn, error)
	// 	Namespace(namespace string) NSConn
	// 	DisconnectAll(ctx context.Context) error

	// 	IsClient() bool
	// 	Server() *Server

	// 	Close()
	// 	IsClosed() bool
	// }

	// NSConn interface {
	// 	Conn() Conn

	// 	Emit(event string, body []byte) bool
	// 	Ask(ctx context.Context, event string, body []byte) (Message, error)

	// 	JoinRoom(ctx context.Context, roomName string) (Room, error)
	// 	Room(roomName string) Room
	// 	LeaveAll(ctx context.Context) error

	// 	Disconnect(ctx context.Context) error
	// }

	// Room interface {
	// 	NSConn() NSConn

	// 	Emit(event string, body []byte) bool
	// 	Leave(ctx context.Context) error
	// }
)

// var (
// 	_ Conn   = (*conn)(nil)
// 	_ NSConn = (*nsConn)(nil)
// 	_ Room   = (*room)(nil)
// )

type Conn struct {
	// the ID generated by `Server#IDGenerator`.
	id string

	// the gorilla or gobwas socket.
	socket Socket

	// non-nil if server-side connection.
	server *Server

	// maximum wait time allowed to read a message from the connection.
	// Defaults to no timeout.
	readTimeout time.Duration
	// maximum wait time allowed to write a message to the connection.
	// Defaults to no timeout.
	writeTimeout time.Duration

	// the defined namespaces, allowed to connect.
	namespaces Namespaces

	// more than 0 if acknowledged.
	acknowledged *uint32

	// the current connection's connected namespace.
	connectedNamespaces      map[string]*NSConn
	connectedNamespacesMutex sync.RWMutex

	// messages that this connection waits for a reply.
	waitingMessages      map[string]chan Message
	waitingMessagesMutex sync.RWMutex

	// used to fire `conn#Close` once.
	closed *uint32
	// useful to terminate the broadcast waiter.
	closeCh chan struct{}
}

func newConn(socket Socket, namespaces Namespaces) *Conn {
	return &Conn{
		socket:              socket,
		namespaces:          namespaces,
		acknowledged:        new(uint32),
		connectedNamespaces: make(map[string]*NSConn),
		waitingMessages:     make(map[string]chan Message),
		closed:              new(uint32),
		closeCh:             make(chan struct{}),
	}
}

func (c *Conn) ID() string {
	return c.id
}

func (c *Conn) String() string {
	return c.ID()
}

func (c *Conn) Socket() Socket {
	return c.socket
}

func (c *Conn) IsClient() bool {
	return c.server == nil
}

func (c *Conn) Server() *Server {
	if c.IsClient() {
		return nil
	}

	return c.server
}

var (
	ackBinary   = []byte("ack")
	ackOKBinary = []byte("ack_ok")
)

func (c *Conn) isAcknowledged() bool {
	return atomic.LoadUint32(c.acknowledged) > 0
}

func (c *Conn) startReader() {
	if c.IsClosed() {
		return
	}
	defer c.Close()

	var (
		queue       = make([]*Message, 0)
		queueMutex  = new(sync.Mutex)
		handleQueue = func() {
			queueMutex.Lock()
			defer queueMutex.Unlock()

			for _, msg := range queue {
				c.handleMessage(*msg)
			}

			queue = nil
		}
	)

	for {
		b, err := c.socket.ReadText(c.readTimeout)
		if err != nil {
			return
		}

		if !c.isAcknowledged() && bytes.HasPrefix(b, ackBinary) {
			if c.IsClient() {
				id := string(b[len(ackBinary):])
				c.id = id
				atomic.StoreUint32(c.acknowledged, 1)
				c.socket.WriteText(ackOKBinary, c.writeTimeout)
				handleQueue()
			} else {
				if len(b) == len(ackBinary) {
					c.socket.WriteText(append(ackBinary, []byte(c.id)...), c.writeTimeout)
				} else {
					// its ackOK, answer from client when ID received and it's ready for write/read.
					atomic.StoreUint32(c.acknowledged, 1)
					handleQueue()
				}
			}

			continue
		}

		msg := deserializeMessage(nil, b)
		if msg.isInvalid {
			// fmt.Printf("%s[%d] is invalid payload\n", b, len(b))
			continue
		}

		if !c.isAcknowledged() {
			queueMutex.Lock()
			queue = append(queue, &msg)
			queueMutex.Unlock()

			continue
		}

		if !c.handleMessage(msg) {
			return
		}
	}
}

func (c *Conn) handleMessage(msg Message) bool {
	if msg.wait != "" {
		c.waitingMessagesMutex.RLock()
		ch, ok := c.waitingMessages[msg.wait]
		c.waitingMessagesMutex.RUnlock()
		if ok {
			ch <- msg
			return true
		}
	}

	switch msg.Event {
	case OnNamespaceConnect:
		c.replyConnect(msg)
	case OnNamespaceDisconnect:
		c.replyDisconnect(msg)
	case OnRoomJoin:
		if ns, ok := c.tryNamespace(msg); ok {
			ns.replyRoomJoin(msg)
		}
	case OnRoomLeave:
		if ns, ok := c.tryNamespace(msg); ok {
			ns.replyRoomLeave(msg)
		}
	default:
		ns, ok := c.tryNamespace(msg)
		if !ok {
			return true
		}

		msg.IsLocal = false
		err := ns.events.fireEvent(ns, msg)
		if err != nil {
			msg.Err = err
			c.Write(msg)
			if isManualCloseError(err) {
				return false // close the connection after sending the closing message.
			}
		}
	}

	return true
}

const syncWaitDur = 15 * time.Millisecond

func (c *Conn) Connect(ctx context.Context, namespace string) (*NSConn, error) {
	if !c.IsClient() {
		for !c.isAcknowledged() {
			time.Sleep(syncWaitDur)
		}
	}

	return c.askConnect(ctx, namespace)
}

// Nil context means try without timeout, wait until it connects to the specific namespace.
// Note that, this function will not return an `ErrBadNamespace` if namespace does not exist in the server-side
// or it's not defined in the client-side, it waits until deadline (if any, or loop forever).
func (c *Conn) WaitConnect(ctx context.Context, namespace string) (ns *NSConn, err error) {
	if ctx == nil {
		ctx = context.TODO()
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if ns == nil {
				ns = c.Namespace(namespace)
			}

			if ns != nil && c.isAcknowledged() {
				return
			}

			time.Sleep(syncWaitDur)
		}
	}
}

func (c *Conn) Namespace(namespace string) *NSConn {
	c.connectedNamespacesMutex.RLock()
	ns := c.connectedNamespaces[namespace]
	c.connectedNamespacesMutex.RUnlock()

	return ns
}

func (c *Conn) tryNamespace(in Message) (*NSConn, bool) {
	ns := c.Namespace(in.Namespace)
	if ns == nil {
		// if _, canConnect := c.namespaces[msg.Namespace]; !canConnect {
		// 	msg.Err = ErrForbiddenNamespace
		// }
		in.Err = ErrBadNamespace
		c.Write(in)
		return nil, false
	}

	return ns, true
}

// server#OnConnected -> conn#Connect
// client#WaitConnect
// or
// client#Connect
func (c *Conn) askConnect(ctx context.Context, namespace string) (*NSConn, error) {
	ns := c.Namespace(namespace)
	if ns != nil {
		return ns, nil
	}

	events, ok := c.namespaces[namespace]
	if !ok {
		return nil, ErrBadNamespace
	}

	connectMessage := Message{
		Namespace: namespace,
		Event:     OnNamespaceConnect,
		IsLocal:   true,
	}

	_, err := c.Ask(ctx, connectMessage) // waits for answer no matter if already connected on the other side.
	if err != nil {
		return nil, err
	}

	// re-check, maybe connected so far (can happen by a simultaneously `Connect` calls on both server and client, which is not the standard way)
	c.connectedNamespacesMutex.RLock()
	ns, ok = c.connectedNamespaces[namespace]
	c.connectedNamespacesMutex.RUnlock()
	if ok {
		return ns, nil
	}

	ns = newNSConn(c, namespace, events)
	err = events.fireEvent(ns, connectMessage)
	if err != nil {
		return nil, err
	}

	c.connectedNamespacesMutex.Lock()
	c.connectedNamespaces[namespace] = ns
	c.connectedNamespacesMutex.Unlock()

	connectMessage.Event = OnNamespaceConnected
	events.fireEvent(ns, connectMessage) // omit error, it's connected.

	return ns, nil
}

func (c *Conn) replyConnect(msg Message) {
	// must give answer even a noOp if already connected.
	if msg.wait == "" || msg.isNoOp {
		return
	}

	ns := c.Namespace(msg.Namespace)
	if ns != nil {
		msg.isNoOp = true
		c.Write(msg)
		return
	}

	events, ok := c.namespaces[msg.Namespace]
	if !ok {
		msg.Err = ErrBadNamespace
		c.Write(msg)
		return
	}

	ns = newNSConn(c, msg.Namespace, events)
	err := events.fireEvent(ns, msg)
	if err != nil {
		msg.Err = err
		c.Write(msg)
		return
	}

	c.connectedNamespacesMutex.Lock()
	c.connectedNamespaces[msg.Namespace] = ns
	c.connectedNamespacesMutex.Unlock()

	c.Write(msg)

	msg.Event = OnNamespaceConnected
	events.fireEvent(ns, msg)
}

func (c *Conn) DisconnectAll(ctx context.Context) error {
	c.connectedNamespacesMutex.Lock()
	defer c.connectedNamespacesMutex.Unlock()

	disconnectMsg := Message{Event: OnNamespaceDisconnect}
	for namespace := range c.connectedNamespaces {
		disconnectMsg.Namespace = namespace
		if err := c.askDisconnect(ctx, disconnectMsg, false); err != nil {
			return err
		}
	}

	return nil
}

func (c *Conn) askDisconnect(ctx context.Context, msg Message, lock bool) error {
	if lock {
		c.connectedNamespacesMutex.RLock()
	}
	ns := c.connectedNamespaces[msg.Namespace]
	if lock {
		c.connectedNamespacesMutex.RUnlock()
	}

	if ns == nil {
		return ErrBadNamespace
	}

	_, err := c.Ask(ctx, msg)
	if err != nil {
		return err
	}

	if lock {
		c.connectedNamespacesMutex.Lock()
	}
	delete(c.connectedNamespaces, msg.Namespace)
	if lock {
		c.connectedNamespacesMutex.Unlock()
	}

	msg.IsLocal = true
	ns.events.fireEvent(ns, msg)

	return nil
}

func (c *Conn) replyDisconnect(msg Message) {
	if msg.wait == "" || msg.isNoOp {
		return
	}

	ns := c.Namespace(msg.Namespace)
	if ns == nil {
		return
	}

	// if client then we need to respond to server and delete the namespace without ask the local event.
	if c.IsClient() {
		c.connectedNamespacesMutex.Lock()
		delete(c.connectedNamespaces, msg.Namespace)
		c.connectedNamespacesMutex.Unlock()
		c.Write(msg)
		ns.events.fireEvent(ns, msg)
		return
	}

	// server-side, check for error on the local event first.
	err := ns.events.fireEvent(ns, msg)
	if err != nil {
		msg.Err = err
	} else {
		c.connectedNamespacesMutex.Lock()
		delete(c.connectedNamespaces, msg.Namespace)
		c.connectedNamespacesMutex.Unlock()
	}
	c.Write(msg)
}

var ErrWrite = fmt.Errorf("write closed")

func (c *Conn) Write(msg Message) bool {
	if c.IsClosed() {
		return false
	}

	// msg.from = c.ID()

	if !msg.isConnect() && !msg.isDisconnect() {
		ns := c.Namespace(msg.Namespace)
		if ns == nil {
			return false
		}

		if msg.Room != "" && !msg.isRoomJoin() && !msg.isRoomLeft() {
			ns.roomsMu.RLock()
			_, ok := ns.rooms[msg.Room]
			ns.roomsMu.RUnlock()
			if !ok {
				// tried to send to a not joined room.
				return false
			}
		}
	}

	err := c.socket.WriteText(serializeMessage(nil, msg), c.writeTimeout)
	if err != nil {
		if IsCloseError(err) {
			c.Close()
		}
		return false
	}

	return true
}

func (c *Conn) Ask(ctx context.Context, msg Message) (Message, error) {
	if c.IsClosed() {
		return msg, CloseError{Code: -1, error: ErrWrite}
	}

	now := time.Now().UnixNano()
	msg.wait = strconv.FormatInt(now, 10)
	if c.IsClient() {
		msg.wait = "client_" + msg.wait
	}

	if ctx == nil {
		ctx = context.TODO()
	} else {
		if deadline, has := ctx.Deadline(); has {
			if deadline.Before(time.Now().Add(-1 * time.Second)) {
				return Message{}, context.DeadlineExceeded
			}
		}
	}

	ch := make(chan Message)
	c.waitingMessagesMutex.Lock()
	c.waitingMessages[msg.wait] = ch
	c.waitingMessagesMutex.Unlock()

	if !c.Write(msg) {
		return Message{}, ErrWrite
	}

	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case receive := <-ch:
		c.waitingMessagesMutex.Lock()
		delete(c.waitingMessages, receive.wait)
		c.waitingMessagesMutex.Unlock()

		return receive, receive.Err
	}
}

func (c *Conn) Close() {
	if atomic.CompareAndSwapUint32(c.closed, 0, 1) {
		close(c.closeCh)
		// fire the namespaces' disconnect event for both server and client.
		disconnectMsg := Message{Event: OnNamespaceDisconnect, IsForced: true, IsLocal: true}
		c.connectedNamespacesMutex.Lock()
		for namespace, ns := range c.connectedNamespaces {
			disconnectMsg.Namespace = ns.namespace
			ns.events.fireEvent(ns, disconnectMsg)
			delete(c.connectedNamespaces, namespace)
		}
		c.connectedNamespacesMutex.Unlock()

		c.waitingMessagesMutex.Lock()
		for wait := range c.waitingMessages {
			delete(c.waitingMessages, wait)
		}
		c.waitingMessagesMutex.Unlock()

		atomic.StoreUint32(c.acknowledged, 0)

		go func() {
			if !c.IsClient() {
				c.server.disconnect <- c
			}
		}()

		c.socket.NetConn().Close()
	}
}

func (c *Conn) IsClosed() bool {
	return atomic.LoadUint32(c.closed) > 0
}
