package main

import (
	"bytes"
	"crypto/tls"
	"encoding/gob"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

func serve(app *config) {

	if app.tls && !fileExists(app.tlsKey) {
		log.Printf("key file not found: %s - disabling TLS", app.tlsKey)
		app.tls = false
	}

	if app.tls && !fileExists(app.tlsCert) {
		log.Printf("cert file not found: %s - disabling TLS", app.tlsCert)
		app.tls = false
	}

	var wg sync.WaitGroup

	for _, h := range app.listeners {
		hh := appendPortIfMissing(h, app.defaultPort)
		listenTCP(app, &wg, hh)
		listenUDP(app, &wg, hh)
	}

	wg.Wait()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func listenTCP(app *config, wg *sync.WaitGroup, h string) {
	log.Printf("listenTCP: TLS=%v spawning TCP listener: %s", app.tls, h)

	// first try TLS
	if app.tls {
		listener, errTLS := listenTLS(app, h)
		if errTLS == nil {
			spawnAcceptLoopTCP(app, wg, listener, true)
			return
		}
		log.Printf("listenTLS: %v", errTLS)
		// TLS failed, try plain TCP
	}

	listener, errListen := net.Listen("tcp", h)
	if errListen != nil {
		log.Printf("listenTCP: TLS=%v %s: %v", app.tls, h, errListen)
		return
	}
	spawnAcceptLoopTCP(app, wg, listener, false)
}

func spawnAcceptLoopTCP(app *config, wg *sync.WaitGroup, listener net.Listener, isTLS bool) {
	wg.Add(1)
	go handleTCP(app, wg, listener, isTLS)
}

func listenTLS(app *config, h string) (net.Listener, error) {
	cert, errCert := tls.LoadX509KeyPair(app.tlsCert, app.tlsKey)
	if errCert != nil {
		log.Printf("listenTLS: failure loading TLS key pair: %v", errCert)
		app.tls = false // disable TLS
		return nil, errCert
	}

	config := &tls.Config{Certificates: []tls.Certificate{cert}}
	listener, errListen := tls.Listen("tcp", h, config)
	return listener, errListen
}

func listenUDP(app *config, wg *sync.WaitGroup, h string) {
	log.Printf("serve: spawning UDP listener: %s", h)

	udpAddr, errAddr := net.ResolveUDPAddr("udp", h)
	if errAddr != nil {
		log.Printf("listenUDP: bad address: %s: %v", h, errAddr)
		return
	}

	conn, errListen := net.ListenUDP("udp", udpAddr)
	if errListen != nil {
		log.Printf("net.ListenUDP: %s: %v", h, errListen)
		return
	}

	wg.Add(1)
	go handleUDP(app, wg, conn)
}

func appendPortIfMissing(host, port string) string {

LOOP:
	for i := len(host) - 1; i >= 0; i-- {
		c := host[i]
		switch c {
		case ']':
			break LOOP
		case ':':
			/*
				if i == len(host)-1 {
					return host[:len(host)-1] + port // drop repeated :
				}
			*/
			return host
		}
	}

	return host + port
}

func handleTCP(app *config, wg *sync.WaitGroup, listener net.Listener, isTLS bool) {
	defer wg.Done()

	var id int

	var aggReader aggregate
	var aggWriter aggregate

	for {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			log.Printf("handle: accept: %v", errAccept)
			break
		}
		go handleConnection(conn, id, 0, isTLS, &aggReader, &aggWriter)
		id++
	}
}

type udpInfo struct {
	remote *net.UDPAddr
	opt    options
	acc    *account
	start  time.Time
	id     int
}

func handleUDP(app *config, wg *sync.WaitGroup, conn *net.UDPConn) {
	defer wg.Done()

	tab := map[string]*udpInfo{}

	buf := make([]byte, app.opt.ReadSize)

	var aggReader aggregate
	var aggWriter aggregate

	var idCount int

	for {
		var info *udpInfo
		n, src, errRead := conn.ReadFromUDP(buf)
		if src == nil {
			log.Printf("handleUDP: read nil src: error: %v", errRead)
			continue
		}
		var found bool
		info, found = tab[src.String()]
		if !found {
			log.Printf("handleUDP: incoming: %v", src)

			info = &udpInfo{
				remote: src,
				acc:    &account{},
				start:  time.Now(),
				id:     idCount,
			}
			idCount++
			info.acc.prevTime = info.start
			tab[src.String()] = info

			dec := gob.NewDecoder(bytes.NewBuffer(buf[:n]))
			if errOpt := dec.Decode(&info.opt); errOpt != nil {
				log.Printf("handleUDP: options failure: %v", errOpt)
				continue
			}
			log.Printf("handleUDP: options received: %v", info.opt)

			if !info.opt.PassiveServer {
				opt := info.opt // copy for gorouting
				go serverWriterTo(conn, opt, src, info.acc, info.id, 0, &aggWriter)
			}

			continue
		}

		connIndex := fmt.Sprintf("%d/%d", info.id, 0)

		if errRead != nil {
			log.Printf("handleUDP: %s read error: %s: %v", connIndex, src, errRead)
			continue
		}

		if time.Since(info.start) > info.opt.TotalDuration {
			log.Printf("handleUDP: total duration %s timer: %s", info.opt.TotalDuration, src)
			info.acc.average(info.start, connIndex, "handleUDP", "rcv/s", &aggReader)
			log.Printf("handleUDP: FIXME: remove idle udp entry from udp table")
			continue
		}

		// account read from UDP socket
		info.acc.update(n, info.opt.ReportInterval, connIndex, "handleUDP", "rcv/s", nil)
	}
}

func handleConnection(conn net.Conn, c, connections int, isTLS bool, aggReader, aggWriter *aggregate) {
	defer conn.Close()

	log.Printf("handleConnection: incoming: %s %v", protoLabel(isTLS), conn.RemoteAddr())

	// receive options
	var opt options
	dec := gob.NewDecoder(conn)
	if errOpt := dec.Decode(&opt); errOpt != nil {
		log.Printf("handleConnection: options failure: %v", errOpt)
		return
	}
	log.Printf("handleConnection: options received: %v", opt)

	// send ack
	a := newAck()
	if errAck := ackSend(false, conn, a); errAck != nil {
		log.Printf("handleConnection: sending ack: %v", errAck)
		return
	}

	latencyReader(conn, opt, c, connections, isTLS, aggReader)

	go serverReader(conn, opt, c, connections, isTLS, aggReader)

	if !opt.PassiveServer {
		go serverWriter(conn, opt, c, connections, isTLS, aggWriter)
	}

	tickerPeriod := time.NewTimer(opt.TotalDuration)

	<-tickerPeriod.C
	log.Printf("handleConnection: %v timer", opt.TotalDuration)

	tickerPeriod.Stop()

	log.Printf("handleConnection: closing: %v", conn.RemoteAddr())
}

func latencyReader(conn net.Conn, opt options, c, connections int, isTLS bool, agg *aggregate) {

	log.Printf("latencyReader: starting: %s %v", protoLabel(isTLS), conn.RemoteAddr())

	//connIndex := fmt.Sprintf("%d/%d", c, connections)

	//var recb bytes.Buffer

	var sendTime uint64
	var sendb bytes.Buffer
	enc := gob.NewEncoder(&sendb)
	dec := gob.NewDecoder(conn)

	for {
		e := dec.Decode(&sendTime)
		if e != nil {
			log.Printf("latencyReader: decode: %v", e)
			break
		}

		if sendTime == 9696969 {
			log.Printf("latencyReader: terminate")
			break
		}

		//timeNow := uint64(time.Now().UnixNano())
		//log.Printf("latencyReader %d, %d",sendTime, timeNow - sendTime)

		enc.Encode(&sendTime)

		_, errWrite := conn.Write(sendb.Bytes())
		if errWrite != nil {
			log.Printf("latencyReader: write: %v", errWrite)
		} else {
			//log.Printf("latencyReader: send %d", sendTime)
		}
		sendb.Reset()
	}

	log.Printf("latencyReader: exiting: %v", conn.RemoteAddr())
}

func serverReader(conn net.Conn, opt options, c, connections int, isTLS bool, agg *aggregate) {

	log.Printf("serverReader: starting: %s %v", protoLabel(isTLS), conn.RemoteAddr())

	connIndex := fmt.Sprintf("%d/%d", c, connections)

	buf := make([]byte, opt.ReadSize)

	workLoop(connIndex, "serverReader", "rcv/s", conn.Read, buf, opt.ReportInterval, 0, nil, agg)

	log.Printf("serverReader: exiting: %v", conn.RemoteAddr())
}

func protoLabel(isTLS bool) string {
	if isTLS {
		return "TLS"
	}
	return "TCP"
}

func serverWriter(conn net.Conn, opt options, c, connections int, isTLS bool, agg *aggregate) {

	log.Printf("serverWriter: starting: %s %v", protoLabel(isTLS), conn.RemoteAddr())

	connIndex := fmt.Sprintf("%d/%d", c, connections)

	buf := randBuf(opt.WriteSize)

	workLoop(connIndex, "serverWriter", "snd/s", conn.Write, buf, opt.ReportInterval, opt.MaxSpeed, nil, agg)

	log.Printf("serverWriter: exiting: %v", conn.RemoteAddr())
}

func serverWriterTo(conn *net.UDPConn, opt options, dst net.Addr, acc *account, c, connections int, agg *aggregate) {
	log.Printf("serverWriterTo: starting: UDP %v", dst)

	start := acc.prevTime

	udpWriteTo := func(b []byte) (int, error) {
		if time.Since(start) > opt.TotalDuration {
			return -1, fmt.Errorf("udpWriteTo: total duration %s timer", opt.TotalDuration)
		}

		return conn.WriteTo(b, dst)
	}

	connIndex := fmt.Sprintf("%d/%d", c, connections)

	buf := randBuf(opt.WriteSize)

	workLoop(connIndex, "serverWriterTo", "snd/s", udpWriteTo, buf, opt.ReportInterval, opt.MaxSpeed, nil, agg)

	log.Printf("serverWriterTo: exiting: %v", dst)
}
