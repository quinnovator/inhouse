package edge

import (
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/tailnet"
)

const (
	// udpIdle is how long a UDP flow may be silent, both ways, before the
	// edge forgets it. WebRTC consent checks run every few seconds, so a
	// live call never idles out.
	udpIdle = 2 * time.Minute
	// maxFlows bounds the UDP flows one listener tracks; packets that would
	// start another are dropped.
	maxFlows = 512
	// maxDatagram fits any UDP payload.
	maxDatagram = 64 << 10
)

// upstream is where a raw listener sends new connections and flows: port on
// 127.0.0.1, published by the revision whose ingress host port is rev.
type upstream struct{ rev, port int }

func (u *upstream) address() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(u.port)) }

// rawListener serves one raw port on a node's tailnet addresses.
type rawListener interface {
	// point sends new connections and flows to u, or drops them when nil.
	// Those already open stay where they started.
	point(u *upstream)
	upstream() *upstream
	// retire closes the connections and flows still open to revision rev.
	retire(rev int)
	close()
}

// listenRaw opens l on n: TCP on every tailnet address, UDP on each of the
// node's IPv4 and IPv6 addresses.
func listenRaw(n *tailnet.Node, l spec.Listener) (rawListener, error) {
	if l.Protocol == spec.TCP {
		ln, err := n.Server.Listen("tcp", ":"+strconv.Itoa(l.Port))
		if err != nil {
			return nil, err
		}
		return serveTCP(ln), nil
	}
	ip4, ip6 := n.Server.TailscaleIPs()
	var pcs []net.PacketConn
	for _, ip := range []netip.Addr{ip4, ip6} {
		if !ip.IsValid() {
			continue
		}
		pc, err := n.Server.ListenPacket("udp", netip.AddrPortFrom(ip, uint16(l.Port)).String())
		if err != nil {
			for _, open := range pcs {
				_ = open.Close()
			}
			return nil, err
		}
		pcs = append(pcs, pc)
	}
	if len(pcs) == 0 {
		return nil, errors.New("node has no tailnet address")
	}
	return serveUDP(pcs), nil
}

// ---- UDP ----

// udpListener forwards datagrams per flow: each remote address gets its own
// connected loopback socket, so the pod sees every peer as a distinct
// address and port, as an ICE agent multiplexing many peers on one port
// needs. Replies on that socket go back to the peer.
type udpListener struct {
	pcs  []net.PacketConn
	live atomic.Pointer[upstream]
	done chan struct{}

	mu     sync.Mutex
	flows  map[string]*udpFlow
	closed bool
}

type udpFlow struct {
	conn *net.UDPConn
	rev  int
	last atomic.Int64 // unix nanoseconds of the last datagram either way
}

func serveUDP(pcs []net.PacketConn) *udpListener {
	l := &udpListener{pcs: pcs, done: make(chan struct{}), flows: map[string]*udpFlow{}}
	for _, pc := range pcs {
		go l.serve(pc)
	}
	go l.expire()
	return l
}

func (l *udpListener) point(u *upstream)   { l.live.Store(u) }
func (l *udpListener) upstream() *upstream { return l.live.Load() }

func (l *udpListener) serve(pc net.PacketConn) {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-l.done:
			default:
				log.Printf("edge: UDP %s stopped: %v", pc.LocalAddr(), err)
			}
			return
		}
		if f := l.flow(pc, from); f != nil {
			f.last.Store(time.Now().UnixNano())
			_, _ = f.conn.Write(buf[:n])
		}
	}
}

// flow returns from's flow, starting one to the live upstream if needed.
func (l *udpListener) flow(pc net.PacketConn, from net.Addr) *udpFlow {
	key := from.String()
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.flows[key]; f != nil {
		return f
	}
	up := l.live.Load()
	if up == nil || l.closed || len(l.flows) >= maxFlows {
		return nil
	}
	addr, err := net.ResolveUDPAddr("udp4", up.address())
	if err != nil {
		return nil
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil
	}
	f := &udpFlow{conn: conn, rev: up.rev}
	f.last.Store(time.Now().UnixNano())
	l.flows[key] = f
	go l.reply(pc, from, key, f)
	return f
}

// reply relays the pod's datagrams to the peer until the flow closes or
// the pod's port refuses them.
func (l *udpListener) reply(pc net.PacketConn, to net.Addr, key string, f *udpFlow) {
	buf := make([]byte, maxDatagram)
	for {
		n, err := f.conn.Read(buf)
		if err != nil {
			break
		}
		f.last.Store(time.Now().UnixNano())
		if _, err = pc.WriteTo(buf[:n], to); err != nil {
			break
		}
	}
	l.mu.Lock()
	if l.flows[key] == f {
		delete(l.flows, key)
	}
	l.mu.Unlock()
	_ = f.conn.Close()
}

func (l *udpListener) expire() {
	tick := time.NewTicker(udpIdle / 4)
	defer tick.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-tick.C:
		}
		cutoff := time.Now().Add(-udpIdle).UnixNano()
		l.closeFlows(func(f *udpFlow) bool { return f.last.Load() < cutoff })
	}
}

// closeFlows closes the flows that match; their reply loops forget them.
func (l *udpListener) closeFlows(match func(*udpFlow) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.flows {
		if match(f) {
			_ = f.conn.Close()
		}
	}
}

func (l *udpListener) retire(rev int) {
	l.closeFlows(func(f *udpFlow) bool { return f.rev == rev })
}

func (l *udpListener) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	close(l.done)
	l.mu.Unlock()
	for _, pc := range l.pcs {
		_ = pc.Close()
	}
	l.closeFlows(func(*udpFlow) bool { return true })
}

// ---- TCP ----

// tcpListener splices each accepted connection to the live upstream.
type tcpListener struct {
	ln   net.Listener
	live atomic.Pointer[upstream]

	mu     sync.Mutex
	conns  map[*tcpConn]bool
	closed bool
}

type tcpConn struct {
	peer, pod net.Conn
	rev       int
}

func (c *tcpConn) close() {
	_ = c.peer.Close()
	_ = c.pod.Close()
}

func serveTCP(ln net.Listener) *tcpListener {
	l := &tcpListener{ln: ln, conns: map[*tcpConn]bool{}}
	go l.serve()
	return l
}

func (l *tcpListener) point(u *upstream)   { l.live.Store(u) }
func (l *tcpListener) upstream() *upstream { return l.live.Load() }

func (l *tcpListener) serve() {
	for {
		peer, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if !closed {
				log.Printf("edge: TCP %s stopped: %v", l.ln.Addr(), err)
			}
			return
		}
		go l.forward(peer)
	}
}

func (l *tcpListener) forward(peer net.Conn) {
	up := l.live.Load()
	if up == nil {
		_ = peer.Close()
		return
	}
	pod, err := net.DialTimeout("tcp4", up.address(), 5*time.Second)
	if err != nil {
		_ = peer.Close()
		return
	}
	c := &tcpConn{peer: peer, pod: pod, rev: up.rev}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		c.close()
		return
	}
	l.conns[c] = true
	l.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go splice(&wg, pod, peer)
	go splice(&wg, peer, pod)
	wg.Wait()
	c.close()
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

// splice copies src to dst, then passes the end of src on as a half-close,
// so protocols that shut down one direction keep working.
func splice(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, err := io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
		if cw.CloseWrite() == nil {
			return
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

func (l *tcpListener) closeConns(match func(*tcpConn) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		if match(c) {
			c.close()
		}
	}
}

func (l *tcpListener) retire(rev int) {
	l.closeConns(func(c *tcpConn) bool { return c.rev == rev })
}

func (l *tcpListener) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	_ = l.ln.Close()
	l.closeConns(func(*tcpConn) bool { return true })
}
