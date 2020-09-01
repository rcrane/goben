package main

import (
	"bytes"
	"crypto/tls"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"reflect"
	"runtime"
	"sync"
	"time"
)

func open(app *config) {

	var proto string
	if app.udp {
		proto = "udp"
	} else {
		proto = "tcp"
	}

	var wg sync.WaitGroup

	var aggReader aggregate
	var aggWriter aggregate

	dialer := net.Dialer{}

	if app.localAddr != "" {
		if app.udp {
			addr, err := net.ResolveUDPAddr(proto, app.localAddr)
			if err != nil {
				log.Printf("open: resolve %s localAddr=%s: %v", proto, app.localAddr, err)
			}
			dialer.LocalAddr = addr
		} else {
			addr, err := net.ResolveTCPAddr(proto, app.localAddr)
			if err != nil {
				log.Printf("open: resolve %s localAddr=%s: %v", proto, app.localAddr, err)
			}
			dialer.LocalAddr = addr
		}
		log.Printf("open: localAddr: %s", dialer.LocalAddr)
	}

	for _, h := range app.hosts {

		hh := appendPortIfMissing(h, app.defaultPort)

		for i := 0; i < app.connections; i++ {

			log.Printf("open: opening TLS=%v %s %d/%d: %s", app.tls, proto, i, app.connections, hh)

			if !app.udp && app.tls {
				// try TLS first
				log.Printf("open: trying TLS")
				conn, errDialTLS := tlsDial(dialer, proto, hh)
				if errDialTLS == nil {
					spawnClient(app, &wg, conn, i, app.connections, true, &aggReader, &aggWriter)
					continue
				}
				log.Printf("open: trying TLS: failure: %s: %s: %v", proto, hh, errDialTLS)
			}

			if !app.udp {
				log.Printf("open: trying non-TLS TCP")
			}

			conn, errDial := dialer.Dial(proto, hh)
			if errDial != nil {
				log.Printf("open: dial %s: %s: %v", proto, hh, errDial)
				continue
			}
			spawnClient(app, &wg, conn, i, app.connections, false, &aggReader, &aggWriter)
		}
	}

	wg.Wait()

	log.Printf("aggregate reading: %d Mbps %d recv/s", aggReader.Mbps, aggReader.Cps)
	log.Printf("aggregate writing: %d Mbps %d send/s", aggWriter.Mbps, aggWriter.Cps)
}

func spawnClient(app *config, wg *sync.WaitGroup, conn net.Conn, c, connections int, isTLS bool, aggReader, aggWriter *aggregate) {
	wg.Add(1)
	go handleConnectionClient(app, wg, conn, c, connections, isTLS, aggReader, aggWriter)
}

func tlsDial(dialer net.Dialer, proto, h string) (net.Conn, error) {
	conf := &tls.Config{
		InsecureSkipVerify: true,
	}

	conn, err := tls.DialWithDialer(&dialer, proto, h, conf)

	return conn, err
}

// ExportInfo records data for export
type ExportInfo struct {
	Input   ChartData
	Output  ChartData
	Latency ChartData
}

func sendOptions(app *config, conn io.Writer) error {
	opt := app.opt
	if app.udp {
		var optBuf bytes.Buffer
		enc := gob.NewEncoder(&optBuf)
		if errOpt := enc.Encode(&opt); errOpt != nil {
			log.Printf("handleConnectionClient: UDP options failure: %v", errOpt)
			return errOpt
		}
		_, optWriteErr := conn.Write(optBuf.Bytes())
		if optWriteErr != nil {
			log.Printf("handleConnectionClient: UDP options write: %v", optWriteErr)
			return optWriteErr
		}
	} else {
		enc := gob.NewEncoder(conn)
		if errOpt := enc.Encode(&opt); errOpt != nil {
			log.Printf("handleConnectionClient: TCP options failure: %v", errOpt)
			return errOpt
		}
	}
	return nil
}

func handleConnectionClient(app *config, wg *sync.WaitGroup, conn net.Conn, c, connections int, isTLS bool, aggReader, aggWriter *aggregate) {
	defer wg.Done()

	log.Printf("handleConnectionClient: starting %s %d/%d %v", protoLabel(isTLS), c, connections, conn.RemoteAddr())

	// send options
	if errOpt := sendOptions(app, conn); errOpt != nil {
		return
	}
	opt := app.opt
	log.Printf("handleConnectionClient: options sent: %v", opt)

	// receive ack
	//log.Printf("handleConnectionClient: FIXME WRITEME server does not send ack for UDP")
	if !app.udp {
		var a ack
		if errAck := ackRecv(app.udp, conn, &a); errAck != nil {
			log.Printf("handleConnectionClient: receiving ack: %v", errAck)
			return
		}
		log.Printf("handleConnectionClient: %s ack received", protoLabel(isTLS))
	}

	doneReader := make(chan struct{})
	doneWriter := make(chan struct{})

	info := ExportInfo{
		Input:   ChartData{},
		Output:  ChartData{},
		Latency: ChartData{},
	}

	var input *ChartData
	var output *ChartData
	var latencydata *ChartData

	if app.csv != "" || app.export != "" || app.chart != "" || app.ascii {
		input = &info.Input
		output = &info.Output
		latencydata = &info.Latency
	}

	clientLatencyTest(conn, c, connections, opt, latencydata)

	go clientReader(conn, c, connections, doneReader, opt, input, aggReader)
	if !app.passiveClient {
		go clientWriter(conn, c, connections, doneWriter, opt, output, aggWriter)
	}

	tickerPeriod := time.NewTimer(app.opt.TotalDuration)

	<-tickerPeriod.C
	log.Printf("handleConnectionClient: %v timer", app.opt.TotalDuration)

	tickerPeriod.Stop()

	conn.Close() // force reader/writer to quit

	<-doneReader // wait reader exit
	if !app.passiveClient {
		<-doneWriter // wait writer exit
	}

	if app.csv != "" {
		filename := fmt.Sprintf(app.csv, c, conn.RemoteAddr())
		log.Printf("exporting CSV test results to: %s", filename)
		errExport := exportCsv(filename, &info)
		if errExport != nil {
			log.Printf("handleConnectionClient: export CSV: %s: %v", filename, errExport)
		}
	}

	if app.export != "" {
		filename := fmt.Sprintf(app.export, c, conn.RemoteAddr())
		log.Printf("exporting YAML test results to: %s", filename)
		errExport := export(filename, &info)
		if errExport != nil {
			log.Printf("handleConnectionClient: export YAML: %s: %v", filename, errExport)
		}
	}

	if app.chart != "" {
		filename := fmt.Sprintf(app.chart, c, conn.RemoteAddr())
		log.Printf("rendering chart to: %s", filename)
		errRender := chartRender(filename, &info.Input, &info.Output, &info.Latency)
		if errRender != nil {
			log.Printf("handleConnectionClient: render PNG: %s: %v", filename, errRender)
		}

		filename = fmt.Sprintf(app.chart, c, conn.RemoteAddr())
		filename = "Lat_" + filename
		errRender = latencyRender(filename, &info.Latency)
		if errRender != nil {
			log.Printf("handleConnectionClient: render PNG: %s: %v", filename, errRender)
		}

	}

	plotascii(&info, conn.RemoteAddr().String(), c)

	log.Printf("handleConnectionClient: closing: %d/%d %v", c, connections, conn.RemoteAddr())
}

func clientLatencyTest(conn net.Conn, c, connections int, opt options, stat *ChartData) {
	log.Printf("clientLatencyTest: starting: %d/%d %v", c, connections, conn.RemoteAddr())

	start := time.Now()
	acc := &account{}
	acc.prevTime = start
	var sendTime uint64

	var sendb bytes.Buffer
	enc := gob.NewEncoder(&sendb)
	dec := gob.NewDecoder(conn)

	for i := 1; i < 50; i++ {

		// send msg
		sendTime := uint64(time.Now().UnixNano())
		errCall := enc.Encode(sendTime)
		if errCall != nil {
			log.Printf("clientLatencyTest encode: %v", errCall)
		}
		_, errWrite := conn.Write(sendb.Bytes())
		if errWrite != nil {
			log.Printf("clientLatencyTest: write error: %v", errWrite)
		} else {
			//log.Printf("clientLatencyTest: send %d %d", n, sendTime)
		}
		sendb.Reset()

		// Read msg
		errCall = dec.Decode(&sendTime)
		if errCall != nil {
			log.Printf("clientLatencyTest decode: %v", errCall)
		} else {
			timeNow := uint64(time.Now().UnixNano())
			duration := timeNow - sendTime
			acc.latencyupdate(float64(duration), stat)
			//log.Printf("clientLatencyTest READ %d, %d", sendTime, duration)
		}
	}

	sendTime = uint64(9696969)
	errCall := enc.Encode(sendTime)
	if errCall != nil {
		log.Printf("clientLatencyTest encode: %v", errCall)
	}
	_, errWrite := conn.Write(sendb.Bytes())
	if errWrite != nil {
		log.Printf("clientLatencyTest: write error: %v", errWrite)
	}

	log.Printf("clientLatencyTest: exiting: %d/%d %v", c, connections, conn.RemoteAddr())
}

func clientReader(conn net.Conn, c, connections int, done chan struct{}, opt options, stat *ChartData, agg *aggregate) {
	log.Printf("clientReader: starting: %d/%d %v", c, connections, conn.RemoteAddr())

	connIndex := fmt.Sprintf("%d/%d", c, connections)

	buf := make([]byte, opt.ReadSize)

	workLoop(connIndex, "clientReader", "rcv/s", conn.Read, buf, opt.ReportInterval, 0, stat, agg)

	close(done)

	log.Printf("clientReader: exiting: %d/%d %v", c, connections, conn.RemoteAddr())
}

func clientWriter(conn net.Conn, c, connections int, done chan struct{}, opt options, stat *ChartData, agg *aggregate) {
	log.Printf("clientWriter: starting: %d/%d %v", c, connections, conn.RemoteAddr())

	connIndex := fmt.Sprintf("%d/%d", c, connections)

	buf := randBuf(opt.WriteSize)

	workLoop(connIndex, "clientWriter", "snd/s", conn.Write, buf, opt.ReportInterval, opt.MaxSpeed, stat, agg)

	close(done)

	log.Printf("clientWriter: exiting: %d/%d %v", c, connections, conn.RemoteAddr())
}

func randBuf(size int) []byte {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("randBuf error: %v", err)
	}
	return buf
}

type call func(p []byte) (n int, err error)

type account struct {
	prevTime  time.Time
	prevSize  int64
	prevCalls int
	size      int64
	calls     int
}

// ChartData records data for chart
type ChartData struct {
	XValues []time.Time
	YValues []float64
}

const fmtReport = "%s %7s %14s rate: %6d Mbps %6d %s"

func (a *account) latencyupdate(n float64, stat *ChartData) {
	a.calls++

	now := time.Now()
	// save chart data
	if stat != nil {
		stat.XValues = append(stat.XValues, now)
		stat.YValues = append(stat.YValues, n)
	}
	//log.Printf("latencyupdate")
}

func (a *account) update(n int, reportInterval time.Duration, conn, label, cpsLabel string, stat *ChartData) {
	a.calls++
	a.size += int64(n)

	now := time.Now()
	elap := now.Sub(a.prevTime)
	if elap > reportInterval {
		elapSec := elap.Seconds()
		mbps := float64(8*(a.size-a.prevSize)) / (1000000 * elapSec)
		cps := int64(float64(a.calls-a.prevCalls) / elapSec)
		log.Printf(fmtReport, conn, "report", label, int64(mbps), cps, cpsLabel)
		a.prevTime = now
		a.prevSize = a.size
		a.prevCalls = a.calls

		// save chart data
		if stat != nil {
			stat.XValues = append(stat.XValues, now)
			stat.YValues = append(stat.YValues, mbps)
		}
	}
}

type aggregate struct {
	Mbps  int64 // Megabit/s
	Cps   int64 // Call/s
	mutex sync.Mutex
}

func (a *account) average(start time.Time, conn, label, cpsLabel string, agg *aggregate) {
	elapSec := time.Since(start).Seconds()
	mbps := int64(float64(8*a.size) / (1000000 * elapSec))
	cps := int64(float64(a.calls) / elapSec)
	log.Printf(fmtReport, conn, "average", label, mbps, cps, cpsLabel)

	agg.mutex.Lock()
	agg.Mbps += mbps
	agg.Cps += cps
	agg.mutex.Unlock()
}

func nameOf(f call) string {
	v := reflect.ValueOf(f)
	log.Printf("workLoop: %s", v)
	if v.Kind() == reflect.Func {
		if rf := runtime.FuncForPC(v.Pointer()); rf != nil {
			return rf.Name()
		}
	}
	return v.String()
}

func workLoop(conn, label, cpsLabel string, f call, buf []byte, reportInterval time.Duration, maxSpeed float64, stat *ChartData, agg *aggregate) {

	start := time.Now()
	acc := &account{}
	acc.prevTime = start

	for {
		runtime.Gosched()

		if maxSpeed > 0 {
			elapSec := time.Since(acc.prevTime).Seconds()
			if elapSec > 0 {
				mbps := float64(8*(acc.size-acc.prevSize)) / (1000000 * elapSec)
				if mbps > maxSpeed {
					time.Sleep(time.Millisecond)
					continue
				}
			}
		}

		n, errCall := f(buf)
		//log.Printf("workLoop: %s", nameOf(f))
		if errCall != nil {
			log.Printf("workLoop: %s %s: %v", conn, label, errCall)
			break
		}
		//log.Printf("workLoop: %s %d", conn, n)
		acc.update(n, reportInterval, conn, label, cpsLabel, stat)
	}

	acc.average(start, conn, label, cpsLabel, agg)
}
