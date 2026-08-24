package warp

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

func startMasqueProxy(psiphonDial func(ctx context.Context, network, addr string) (net.Conn, error), port string, logger *log.Logger) {
	startTCPForwarder(psiphonDial, "162.159.198.2:443", "127.0.0.1:"+port, logger)
}

func startTCPForwarder(psiphonDial func(ctx context.Context, network, addr string) (net.Conn, error), targetAddr, listenAddr string, logger *log.Logger) {
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Printf("MASQUE proxy listen %s: %v", listenAddr, err)
		return
	}

	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go handleForward(client, psiphonDial, targetAddr, logger)
		}
	}()

	logger.Printf("MASQUE proxy: %s -> %s", listenAddr, targetAddr)
}

func handleForward(client net.Conn, psiphonDial func(ctx context.Context, network, addr string) (net.Conn, error), target string, logger *log.Logger) {
	defer client.Close()

	var remote net.Conn
	var err error

	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		remote, err = psiphonDial(ctx, "tcp", target)
		cancel()
		if err == nil {
			break
		}
		if i == 0 {
			logger.Printf("MASQUE proxy: waiting for Psiphon tunnel...")
		}
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		logger.Printf("MASQUE proxy: Psiphon failed (%v), using direct", err)
		remote, err = net.DialTimeout("tcp", target, 15*time.Second)
		if err != nil {
			logger.Printf("MASQUE proxy direct dial: %v", err)
			return
		}
	}

	defer remote.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(remote, client); wg.Done(); remote.Close() }()
	go func() { io.Copy(client, remote); wg.Done(); client.Close() }()
	wg.Wait()
}
