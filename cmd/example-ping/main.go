package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/m-lab/go/rtx"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

/*

err := http2.ConfigureTransport(tr)

client := http.Client{Transport: tr}
r, err := client.Get(fmt.Sprintf("https://localhost:%d", port))

c <- os.Interrupt

if err != nil {
	t.Fatalf("Error encountered while connecting to test server: %s", err)
}

if r.Proto != "HTTP/2.0" {
	t.Fatalf("Expected HTTP/2 connection to server, but connection was using %s", r.Proto)
}
*/

var (
	ready = make(chan bool)
)

func connect(host, addr string) (*http2.Framer, error) {
	cfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"h2", "h2-14"},
		InsecureSkipVerify: false,
	}

	hostAndPort := addr
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
	log.Printf("Sending client preface: %q", http2.ClientPreface)
	if _, err := io.WriteString(tc, http2.ClientPreface); err != nil {
		return nil, err
	}

	framer := http2.NewFramer(tc, tc)
	framer.SetMaxReadFrameSize((1 << 24) - 1)
	s := []http2.Setting{
		http2.Setting{ID: http2.SettingMaxFrameSize, Val: (1 << 17)},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: (1 << 30) - 1},
	}
	log.Println("Sending settings: ", s)
	err = framer.WriteSettings(s...)
	rtx.Must(err, "failed to write settings")
	//log.Println("Sending settings ack (presumptively)")
	//err = framer.WriteSettingsAck()
	//rtx.Must(err, "failed to write settings ack")
	return framer, nil
}

func request(framer *http2.Framer) {
	log.Println("Waiting for SETTINGS(ack)")
	<-ready // block until pinger receives frame.

	buf := bytes.Buffer{}
	enc := hpack.NewEncoder(&buf)
	enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
	enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	enc.WriteField(hpack.HeaderField{Name: ":path", Value: "/large"})
	block := buf.Bytes()

	p := http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block,
		EndStream:     false,
		EndHeaders:    true,
	}
	log.Println("Sending headers request: ", p, string(p.BlockFragment))
	err := framer.WriteHeaders(p)
	rtx.Must(err, "failed to write headers for stream 1")

	log.Println("WriteWindowUpdate")
	err = framer.WriteWindowUpdate(0, 1<<30)
	rtx.Must(err, "failed to update window 0")
	err = framer.WriteWindowUpdate(1, 1<<30)
	rtx.Must(err, "failed to update window 1")

	//return framer, nil
}

func ping(framer *http2.Framer) {
	// err := framer.WriteSettingsAck()
	// rtx.Must(err, "failed to write settings ack")
	/*
		s := []http2.Setting{
			http2.Setting{ID: http2.SettingInitialWindowSize, Val: (2 << 21)},
			http2.Setting{ID: http2.SettingMaxFrameSize, Val: (2 << 22)},
		}
		err = framer.WriteSettings(s...)
		rtx.Must(err, "failed to write settings")
	*/
	total := 0
	t1 := time.Now()
	start := time.Now()
	once := sync.Once{}
	go func() {
		for {
			f, err := framer.ReadFrame()
			rtx.Must(err, "failed to read frame")
			switch f := f.(type) {
			case *http2.PingFrame:
				log.Printf("  PING = %q %s %f Mbps %f MB %f sec\n",
					f.Data, time.Since(t1),
					float64(total)/time.Since(start).Seconds()/1000/1000,
					float64(total)/1000/1000,
					time.Since(start).Seconds())
			case *http2.DataFrame:
				fmt.Println("  DATA = ", time.Since(t1), f.Length, f.Type, f.StreamID, f.StreamEnded(), f.FrameHeader.String())
				// fmt.Println(string(f.Data()))
				total += int(f.Length)
				err = framer.WriteWindowUpdate(1, f.Length)
				if err != nil {
					log.Println("  - window update:", err)
				}
				err = framer.WriteWindowUpdate(0, f.Length)
				if err != nil {
					log.Println("  - window update:", err)
				}
			case *http2.SettingsFrame:
				log.Println("  SETTINGS = ", time.Since(t1), f.String())
				f.ForeachSetting(func(s http2.Setting) error { log.Println("  - ", s); return nil })
				if !f.IsAck() {
					log.Println("Sending SETTINGS(ack) ...")
					err = framer.WriteSettingsAck()
					rtx.Must(err, "failed to write settings ack")
				}
				once.Do(func() {
					ready <- true
					//log.Println("ready")
				})
			case *http2.WindowUpdateFrame:
				log.Println("  WINDOW_UPDATE = ", f.String(), f.Increment)
			case *http2.HeadersFrame:
				log.Println("  HEADERS = ", f.String())
				b := f.HeaderBlockFragment()
				dec := hpack.NewDecoder(4096, nil)
				dec.SetEmitFunc(func(hf hpack.HeaderField) {
					log.Println("  - Name:", hf.Name, "Value:", hf.Value)
				})
				dec.Write(b)
				rtx.Must(dec.Close(), "closed decoder")
			default:
				log.Println("frame:", f)
				// pretty.Print(f)
				log.Println()
			}
		}
	}()

	var msg [8]byte
	copy(msg[:], "h2i_ping")

	/*	f, err := framer.ReadFrame()
		fmt.Println(f)
		rtx.Must(err, "failed to read 1 frame ")

		f, err = framer.ReadFrame()
		fmt.Println(f)
		rtx.Must(err, "failed to read 2 frames")

		err = framer.WritePing(false, msg)
		rtx.Must(err, "failed to write ping")
	*/

	//f, err = framer.ReadFrame()
	//rtx.Must(err, "failed to read response")
	//fmt.Println(f)
	var err error
	for {
		//err = framer.WriteWindowUpdate(1, 2^20)
		//fmt.Println("window update:", err)

		time.Sleep(2 * time.Second)
		t1 = time.Now()
		err = framer.WritePing(false, msg)
		rtx.Must(err, "failed to write ping")
		//fmt.Println("  loop = ", time.Since(t1))
	}
}

/*
	f(":authority", host)
	m := req.Method
	if m == "" {
		m = MethodGet
	}
	f(":method", m)
	if req.Method != "CONNECT" {
		f(":path", path)
		f(":scheme", req.URL.Scheme)
	}
*/

func main() {

	fmt.Println("ok")
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err := http2.ConfigureTransport(tr)
	rtx.Must(err, "failed to configure http transport")

	req := &http.Request{
		Method: "CONNECT",
		URL: &url.URL{
			Scheme: "https",
			Host:   "mlab1-lga0t.mlab-sandbox.measurement-lab.org:443",
		},
	}

	f, err := connect("mlab1-lga0t.mlab-sandbox.measurement-lab.org", "mlab1-lga0t.mlab-sandbox.measurement-lab.org:443")
	rtx.Must(err, "failed to connect")
	// go read(f)
	go request(f)
	ping(f)

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
