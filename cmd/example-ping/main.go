package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/m-lab/go/rtx"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	host = flag.String("host", "", "hostname of target http2 server")
	addr = flag.String("addr", "443", "port number to connect to")
	path = flag.String("path", "/large", "resource on target server to GET")
)

var (
	ready = make(chan bool)
)

func connect(host, port string) (*http2.Framer, error) {
	cfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"h2", "h2-14"},
		InsecureSkipVerify: true,
	}

	hostAndPort := host + ":" + port
	log.Printf("Connecting to %s ...", hostAndPort)
	tc, err := tls.Dial("tcp", hostAndPort, cfg)
	if err != nil {
		return nil, fmt.Errorf("Error dialing %s: %v", hostAndPort, err)
	}
	log.Printf("Connected to %v", tc.RemoteAddr())
	//defer tc.Close()

	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS handshake: %v", err)
	}
	/*
		if err := tc.VerifyHostname(app.host); err != nil {
			return fmt.Errorf("VerifyHostname: %v", err)
		}
	*/
	state := tc.ConnectionState()
	log.Printf("Negotiated protocol %q", state.NegotiatedProtocol)
	if state.NegotiatedProtocol == "" {
		return nil, fmt.Errorf("could not negotiate protocol mutually")
	}

	// Start HTTP/2 connection.
	log.Printf("SEND preface: %q", http2.ClientPreface)
	if _, err := io.WriteString(tc, http2.ClientPreface); err != nil {
		return nil, err
	}

	framer := http2.NewFramer(tc, tc)
	framer.SetMaxReadFrameSize((1 << 24) - 1)
	s := []http2.Setting{
		http2.Setting{ID: http2.SettingMaxFrameSize, Val: (1 << 24) - 1},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: (1 << 30) - 1},
	}
	log.Println("SEND settings: ", s)
	err = framer.WriteSettings(s...)
	rtx.Must(err, "failed to write settings")
	//log.Println("Sending settings ack (presumptively)")
	//err = framer.WriteSettingsAck()
	//rtx.Must(err, "failed to write settings ack")
	return framer, nil
}

func request(framer *http2.Framer) {
	log.Println("WAIT - SETTINGS(ack)")
	<-ready // block until pinger receives frame.

	buf := bytes.Buffer{}
	enc := hpack.NewEncoder(&buf)
	enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
	enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	enc.WriteField(hpack.HeaderField{Name: ":path", Value: *path})
	block := buf.Bytes()

	p := http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block,
		EndStream:     false,
		EndHeaders:    true,
	}
	log.Println("SEND - request header: ", p)
	err := framer.WriteHeaders(p)
	rtx.Must(err, "failed to write headers for stream 1")

	log.Println("SEND - WriteWindowUpdate(0)")
	err = framer.WriteWindowUpdate(0, (1<<30)-1)
	rtx.Must(err, "failed to update window 0")
	err = framer.WriteWindowUpdate(1, (1<<30)-1)
	rtx.Must(err, "failed to update window 1")
}

func ping(framer *http2.Framer) {
	total := 0
	// t1 := time.Now()
	start := time.Now()
	once := sync.Once{}
	pings := []time.Time{}
	mx := sync.Mutex{}
	go func() {
		for {
			f, err := framer.ReadFrame()
			rtx.Must(err, "failed to read frame")
			switch f := f.(type) {
			case *http2.PingFrame:
				if f.IsAck() {
					mx.Lock()
					t1 := pings[0]
					pings = pings[1:]
					mx.Unlock()
					log.Printf("  RECV PING = %q %s %f Mbps %f MB %f sec\n",
						f.Data, time.Since(t1),
						8*float64(total)/time.Since(start).Seconds()/1000/1000,
						float64(total)/1000/1000,
						time.Since(start).Seconds())
				}
			case *http2.DataFrame:
				total += int(f.Length)
				// fmt.Println("  DATA = ", f.Length, f.Type, "stream:", f.StreamID, f.StreamEnded(), f.FrameHeader.String(), total)
				if f.StreamEnded() {
					break
				}
				go func(stream, length uint32) {
					mx.Lock()
					// TODO: why are both necessary?
					err = framer.WriteWindowUpdate(0, length)
					if err != nil {
						log.Println("  - window update:", err)
					}
					err = framer.WriteWindowUpdate(stream, length)
					if err != nil {
						log.Println("  - window update:", err)
					}
					mx.Unlock()
				}(f.StreamID, f.Length)
			case *http2.SettingsFrame:
				log.Println("  RECV SETTINGS = ", f.String())
				f.ForeachSetting(func(s http2.Setting) error { log.Println("  - ", s); return nil })
				if !f.IsAck() {
					log.Println("SEND - SETTINGS(ack) ...")
					err = framer.WriteSettingsAck()
					rtx.Must(err, "failed to write settings ack")
				}
				once.Do(func() {
					ready <- true
					//log.Println("ready")
				})
			case *http2.WindowUpdateFrame:
				log.Println("  RECV - WINDOW_UPDATE = ", f.String(), f.Increment)
			case *http2.HeadersFrame:
				log.Println("  RECV - HEADERS = ", f.String())
				b := f.HeaderBlockFragment()
				dec := hpack.NewDecoder(4096, nil)
				dec.SetEmitFunc(func(hf hpack.HeaderField) {
					log.Println("  - Name:", hf.Name, "Value:", hf.Value)
				})
				dec.Write(b)
				rtx.Must(dec.Close(), "closed decoder")
			default:
				log.Printf("RECV - %T %s", f, f)
				log.Println()
			}
		}
	}()

	var msg [8]byte
	copy(msg[:], "h2i_ping")

	var err error
	for {
		time.Sleep(100 * time.Millisecond)
		mx.Lock()
		pings = append(pings, time.Now())
		err = framer.WritePing(false, msg)
		mx.Unlock()
		if err != nil {
			//	rtx.Must(err, "failed to write ping")
			fmt.Println("failed to write ping:", err)
		}
	}
}

func main() {
	flag.Parse()
	fmt.Println("ok")

	// f, err := connect("mlab1-lga0t.mlab-sandbox.measurement-lab.org", "443")
	f, err := connect(*host, *addr)
	rtx.Must(err, "failed to connect")
	go request(f)
	ping(f)

	/*
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		err := http2.ConfigureTransport(tr)
		rtx.Must(err, "failed to configure http transport")
	*/
	/*
		req := &http.Request{
			Method: "CONNECT",
			URL: &url.URL{
				Scheme: "https",
				Host:   "mlab1-lga0t.mlab-sandbox.measurement-lab.org:443",
			},
		}

		client := http.Client{Transport: tr}
		// r, err := client.Get("https://ndt-mlab1-lga0t.mlab-sandbox.measurement-lab.org:443")
		r, err := client.Do(req)
		rtx.Must(err, "failed to get url")
		_, ok := r.Body.(net.Conn)
		if !ok {
			panic("not a net.Conn")
		}
		//c, err := net.Dial("tcp", "ndt-mlab1-lga0t.mlab-sandbox.measurement-lab.org:443")
		//c, err := net.Dial("tcp", "mlab-ns.appspot.com:80")
		// rtx.Must(err, "failed to dail node.")
		if r.Proto != "HTTP/2.0" {
			fmt.Println("wrong proto: ", r.Proto)
		}
		fmt.Println("ok")
		//cc, err := tr2.NewClientConn(c)
		//rtx.Must(err, "failed to create tr2 client conn")
	*/
	/*
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for err == nil {
			t1 := time.Now()
			err = cc.Ping(ctx)
			d := time.Since(t1)
			fmt.Println("ping:", d)
		}
		fmt.Println(err)
	*/
}
