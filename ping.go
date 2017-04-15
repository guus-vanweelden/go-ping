package ping

import (
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	timeSliceLength  = 8
	protocolICMP     = 1
	protocolIPv6ICMP = 58
)

var (
	ipv4Proto = map[string]string{"ip": "ip4:icmp", "udp": "udp4"}
	ipv6Proto = map[string]string{"ip": "ip6:ipv6-icmp", "udp": "udp6"}
)

// NewPinger returns a new Pinger struct pointer
func NewPinger(addr string) (*Pinger, error) {
	ipaddr, err := net.ResolveIPAddr("ip", addr)
	if err != nil {
		return nil, err
	}

	return &Pinger{
		ipaddr:   ipaddr,
		addr:     addr,
		Interval: time.Second,
		Timeout:  15 * time.Second,
		Count:    -1,

		network: "udp",
		ipv4:    isIPv4(ipaddr.IP),
		size:    timeSliceLength,

		done: make(chan bool),
	}, nil
}

// Pinger represents ICMP packet sender/receiver
type Pinger struct {
	// Interval is the wait time between each packet send. Default is 1s.
	Interval time.Duration

	// Timeout specifies a timeout before ping exits, regardless of how many
	// packets have been received.
	Timeout time.Duration

	// Count tells pinger to stop after sending (and receiving) Count echo
	// packets. If this option is not specified, pinger will operate until
	// interrupted.
	Count int

	// Number of packets sent
	PacketsSent int

	// Number of packets received
	PacketsRecv int

	// rtts is all of the Rtts
	rtts []time.Duration

	// OnRecv is called when Pinger receives and processes a packet
	OnRecv func(*Packet)

	// OnFinish is called when Pinger exits
	OnFinish func(*Statistics)

	// stop chan bool
	done chan bool

	ipaddr *net.IPAddr
	addr   string

	ipv4     bool
	source   string
	size     int
	sequence int
	network  string
}

// Packet represents a received and processed ICMP echo packet.
type Packet struct {
	// Rtt is the round-trip time it took to ping.
	Rtt time.Duration

	// IPAddr is the address of the host being pinged.
	IPAddr *net.IPAddr

	// NBytes is the number of bytes in the message.
	Nbytes int

	// Seq is the ICMP sequence number.
	Seq int
}

// Statistics represent the stats of a currently running or finished
// pinger operation.
type Statistics struct {
	// PacketsRecv is the number of packets received.
	PacketsRecv int

	// PacketsSent is the number of packets sent.
	PacketsSent int

	// PacketLoss is the percentage of packets lost.
	PacketLoss float64

	// IPAddr is the address of the host being pinged.
	IPAddr *net.IPAddr

	// Addr is the string address of the host being pinged.
	Addr string

	// Rtts is all of the round-trip times sent via this pinger.
	Rtts []time.Duration

	// MinRtt is the minimum round-trip time sent via this pinger.
	MinRtt time.Duration

	// MaxRtt is the maximum round-trip time sent via this pinger.
	MaxRtt time.Duration

	// AvgRtt is the average round-trip time sent via this pinger.
	AvgRtt time.Duration

	// StdDevRtt is the standard deviation of the round-trip times sent via
	// this pinger.
	StdDevRtt time.Duration
}

// SetIPAddr sets the ip address of the target host.
func (p *Pinger) SetIPAddr(ipaddr *net.IPAddr) {
	p.ipaddr = ipaddr
	p.addr = ipaddr.String()
	p.ipv4 = isIPv4(ipaddr.IP)
}

// IPAddr returns the ip address of the target host.
func (p *Pinger) IPAddr() *net.IPAddr { return p.ipaddr }

// SetAddr resolves and sets the ip address of the target host, addr can be a
// DNS name like "www.google.com" or IP like "127.0.0.1".
func (p *Pinger) SetAddr(addr string) error {
	ipaddr, err := net.ResolveIPAddr("ip", addr)
	if err != nil {
		return err
	}

	p.SetIPAddr(ipaddr)
	p.addr = addr
	return nil
}

// Addr returns the string ip address of the target host.
func (p *Pinger) Addr() string { return p.addr }

// SetPrivileged sets the type of ping pinger will send.
// false means pinger will send an "unprivileged" UDP ping.
// true means pinger will send a "privileged" raw ICMP ping.
// NOTE: setting to true requires that it be run with super-user privileges.
func (p *Pinger) SetPrivileged(privileged bool) {
	p.network = "udp"
	if privileged {
		p.network = "ip"
	}
}

// Privileged returns whether pinger is running in privileged mode.
func (p *Pinger) Privileged() bool { return p.network == "ip" }

// Run runs the pinger. This is a blocking function that will exit when it's
// done. If Count or Interval are not specified, it will run continuously until
// it is interrupted.
func (p *Pinger) Run() { p.run() }

func (p *Pinger) run() {
	var conn *icmp.PacketConn
	if p.ipv4 {
		if conn = p.listen(ipv4Proto[p.network], p.source); conn == nil {
			return
		}
	} else {
		if conn = p.listen(ipv6Proto[p.network], p.source); conn == nil {
			return
		}
	}
	defer conn.Close()
	defer p.finish()

	var wg sync.WaitGroup
	recv := make(chan *packet, 5)
	wg.Add(1)
	go p.recvICMP(conn, recv, &wg)

	err := p.sendICMP(conn)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	timeout := time.NewTicker(p.Timeout)
	interval := time.NewTicker(p.Interval)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)

	for {
		select {
		case <-c:
			close(p.done)
		case <-p.done:
			wg.Wait()
			return
		case <-timeout.C:
			close(p.done)
			wg.Wait()
			return
		case <-interval.C:
			err = p.sendICMP(conn)
			if err != nil {
				fmt.Println("FATAL: ", err.Error())
			}
		case r := <-recv:
			err := p.processPacket(r)
			if err != nil {
				fmt.Println("FATAL: ", err.Error())
			}
		default:
			if p.Count > 0 && p.PacketsRecv >= p.Count {
				close(p.done)
				wg.Wait()
				return
			}
		}
	}
}

func (p *Pinger) finish() {
	if p.OnFinish != nil {
		p.OnFinish(p.Statistics())
	}
}

// Statistics returns the statistics of the pinger. This can be run while the
// pinger is running or after it is finished. OnFinish calls this function to
// get it's finished statistics.
func (p *Pinger) Statistics() *Statistics {
	loss := float64(p.PacketsSent-p.PacketsRecv) / float64(p.PacketsSent) * 100
	var min, max, total time.Duration
	if len(p.rtts) > 0 {
		min = p.rtts[0]
		max = p.rtts[0]
	}
	for _, rtt := range p.rtts {
		if rtt < min {
			min = rtt
		}
		if rtt > max {
			max = rtt
		}
		total += rtt
	}
	s := Statistics{
		PacketsSent: p.PacketsSent,
		PacketsRecv: p.PacketsRecv,
		PacketLoss:  loss,
		Rtts:        p.rtts,
		Addr:        p.addr,
		IPAddr:      p.ipaddr,
		MaxRtt:      max,
		MinRtt:      min,
	}
	if len(p.rtts) > 0 {
		s.AvgRtt = total / time.Duration(len(p.rtts))
		var sumsquares time.Duration
		for _, rtt := range p.rtts {
			sumsquares += (rtt - s.AvgRtt) * (rtt - s.AvgRtt)
		}
		s.StdDevRtt = time.Duration(math.Sqrt(
			float64(sumsquares / time.Duration(len(p.rtts)))))
	}
	return &s
}

func (p *Pinger) recvICMP(
	conn *icmp.PacketConn,
	recv chan<- *packet,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	for {
		select {
		case <-p.done:
			return
		default:
			bytes := make([]byte, 512)
			conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, _, err := conn.ReadFrom(bytes)
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						// Read timeout
						continue
					} else {
						close(p.done)
						return
					}
				}
			}
			recv <- &packet{bytes: bytes, nbytes: n}
		}
	}
}

func (p *Pinger) processPacket(recv *packet) error {
	var (
		bytes []byte
		proto int
	)
	if p.ipv4 {
		if p.network == "ip" {
			bytes = ipv4Payload(recv.bytes)
		} else {
			bytes = recv.bytes
		}
		proto = protocolICMP
	} else {
		bytes = recv.bytes
		proto = protocolIPv6ICMP
	}

	var m *icmp.Message
	var err error
	if m, err = icmp.ParseMessage(proto, bytes[:recv.nbytes]); err != nil {
		return fmt.Errorf("Error parsing icmp message")
	}

	if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply {
		// Not an echo reply, ignore it
		return nil
	}

	outPkt := &Packet{
		Nbytes: recv.nbytes,
		IPAddr: p.ipaddr,
	}

	switch pkt := m.Body.(type) {
	case *icmp.Echo:
		outPkt.Rtt = time.Since(bytesToTime(pkt.Data[:timeSliceLength]))
		outPkt.Seq = pkt.Seq
		p.PacketsRecv++
	default:
		// Very bad, not sure how this can happen
		return fmt.Errorf("Error, invalid ICMP echo reply. Body type: %T, %s",
			pkt, pkt)
	}

	p.rtts = append(p.rtts, outPkt.Rtt)
	if p.OnRecv != nil {
		p.OnRecv(outPkt)
	}
	return nil
}

func (p *Pinger) sendICMP(conn *icmp.PacketConn) error {
	var typ icmp.Type = ipv6.ICMPTypeEchoRequest
	if p.ipv4 {
		typ = ipv4.ICMPTypeEcho
	}

	var dst net.Addr = p.ipaddr
	if p.network == "udp" {
		dst = &net.UDPAddr{IP: p.ipaddr.IP, Zone: p.ipaddr.Zone}
	}

	t := timeToBytes(time.Now())
	if p.size-timeSliceLength != 0 {
		t = append(t, byteSliceOfSize(p.size-timeSliceLength)...)
	}
	bytes, err := (&icmp.Message{
		Type: typ, Code: 0,
		Body: &icmp.Echo{
			ID:   rand.Intn(65535),
			Seq:  p.sequence,
			Data: t,
		},
	}).Marshal(nil)
	if err != nil {
		return err
	}

	for {
		if _, err := conn.WriteTo(bytes, dst); err != nil {
			if neterr, ok := err.(*net.OpError); ok {
				if neterr.Err == syscall.ENOBUFS {
					continue
				}
			}
		}
		p.PacketsSent++
		p.sequence++
		break
	}
	return nil
}

func (p *Pinger) listen(netProto string, source string) *icmp.PacketConn {
	conn, err := icmp.ListenPacket(netProto, source)
	if err != nil {
		fmt.Printf("Error listening for ICMP packets: %s\n", err.Error())
		close(p.done)
		return nil
	}
	return conn
}
