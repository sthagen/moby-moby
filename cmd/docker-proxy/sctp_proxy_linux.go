package main

import (
	"io"
	"log"
	"net"
	"sync"

	"github.com/ishidawataru/sctp"
)

// SCTPProxy is a proxy for SCTP connections. It implements the Proxy interface to
// handle SCTP traffic forwarding between the frontend and backend addresses.
type SCTPProxy struct {
	listener     *sctp.SCTPListener
	frontendAddr *sctp.SCTPAddr
	backendAddr  *sctp.SCTPAddr
}

// NewSCTPProxy creates a new SCTPProxy.
func NewSCTPProxy(listener *sctp.SCTPListener, backendAddr *sctp.SCTPAddr) (*SCTPProxy, error) {
	return &SCTPProxy{
		listener:     listener,
		frontendAddr: listener.Addr().(*sctp.SCTPAddr),
		backendAddr:  backendAddr,
	}, nil
}

func (proxy *SCTPProxy) clientLoop(client *sctp.SCTPConn, quit chan bool) {
	backend, err := sctp.DialSCTP("sctp", nil, proxy.backendAddr)
	if err != nil {
		log.Printf("Can't forward traffic to backend sctp/%v: %s\n", proxy.backendAddr, err)
		client.Close()
		return
	}
	clientC := sctp.NewSCTPSndRcvInfoWrappedConn(client)
	backendC := sctp.NewSCTPSndRcvInfoWrappedConn(backend)

	var wg sync.WaitGroup
	broker := func(to, from net.Conn) {
		io.Copy(to, from)
		from.Close()
		to.Close()
		wg.Done()
	}

	wg.Add(2)
	go broker(clientC, backendC)
	go broker(backendC, clientC)

	finish := make(chan struct{})
	go func() {
		wg.Wait()
		close(finish)
	}()

	select {
	case <-quit:
	case <-finish:
	}
	clientC.Close()
	backendC.Close()
	<-finish
}

// Run starts forwarding the traffic using SCTP.
func (proxy *SCTPProxy) Run() {
	quit := make(chan bool)
	defer close(quit)
	for {
		client, err := proxy.listener.Accept()
		if err != nil {
			log.Printf("Stopping proxy on sctp/%v for sctp/%v (%s)", proxy.frontendAddr, proxy.backendAddr, err)
			return
		}
		go proxy.clientLoop(client.(*sctp.SCTPConn), quit)
	}
}

// Close stops forwarding the traffic.
func (proxy *SCTPProxy) Close() { proxy.listener.Close() }
