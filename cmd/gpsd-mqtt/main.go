package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
)

func resolveMDNS(host string) ([]net.IP, error) {
	mCast := &net.UDPAddr{
		IP:   net.ParseIP("224.0.0.251"),
		Port: 5353,
	}

	uConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer uConn.Close()

	msg := new(dns.Msg)
	name := dns.Fqdn(host)
	msg.SetQuestion(name, dns.TypeA)
	data, err := msg.Pack()
	if err != nil {
		return nil, err
	}

	_, err = uConn.WriteToUDP(data, mCast)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 65536)
	n, err := uConn.Read(buf)
	if err != nil {
		return nil, err
	}

	err = msg.Unpack(buf[:n])
	if err != nil {
		return nil, err
	}

	var result []net.IP
	for _, r := range msg.Answer {
		a, ok := r.(*dns.A)
		if !ok {
			continue
		}
		if a.Hdr.Name != name {
			continue
		}

		result = append(result, a.A)
	}

	return result, nil
}
func resolveLookup(hostport string) ([]string, error) {
	host, port, _ := net.SplitHostPort(hostport)
	if port == "" {
		port = "1883"
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		addrs, err = resolveMDNS(host)
	}
	if err != nil {
		return nil, err
	}

	var result []string
	for _, ip := range addrs {
		result = append(result, ip.String()+":"+port)
	}

	return result, nil
}
func mdnsLookup(service string) ([]string, error) {
	var results []string
	ch := make(chan *mdns.ServiceEntry)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range ch {
			if e.AddrV4 == nil || e.AddrV4.IsUnspecified() {
				continue
			}
			log.Println("Found MQTT server:", e.Host, e.AddrV4)
			results = append(results, fmt.Sprintf("%s:%d", e.AddrV4.String(), e.Port))
		}
	}()

	err := mdns.Lookup(service, ch)
	close(ch)
	wg.Wait()
	return results, err
}

type goClient struct {
	mqtt.Client
}

func goify(tok mqtt.Token) error {
	tok.Wait()
	return tok.Error()
}
func (c *goClient) Connect() error { return goify(c.Client.Connect()) }

func (c *goClient) Publish(topic string, qos int, retain bool, payload []byte) error {
	topic = strings.TrimPrefix(topic, "/")
	log.Println("Publish:", topic, string(payload))
	return goify(c.Client.Publish(topic, byte(qos), retain, payload))
}

func main() {
	log.SetFlags(log.Lshortfile)
	connStr := flag.String("s", "tcp:///gpsd", "Connection url, leave host blank to use service discovery.")
	gpsd := flag.String("g", ":2947", "GPSD connection address.")
	id := flag.String("c", "gpsd-mqtt", "Client ID.")
	flag.Parse()

	u, err := url.Parse(*connStr)
	if err != nil {
		log.Fatal("ERROR: invalid server url: ", err)
	}
	topic := strings.TrimPrefix(u.Path, "/")
	u.Path = ""

	opts := mqtt.NewClientOptions()
	opts.SetClientID(*id)
	opts.SetWill(path.Join(topic, "online"), "false", 1, true)
	var hosts []string
	if u.Host != "" {
		hosts, err = resolveLookup(u.Host)
		if err != nil {
			log.Fatal("ERROR: resolve lookup: ", err)
		}
	} else {
		hosts, err = mdnsLookup("_mqtt._tcp")
		if err != nil {
			log.Fatal("ERROR: mdns lookup: ", err)
		}
	}

	for _, u.Host = range hosts {
		log.Println("Adding broker:", u.String())
		opts.AddBroker(u.String())
	}

	cli := goClient{Client: mqtt.NewClient(opts)}
	if err := cli.Connect(); err != nil {
		log.Fatal("ERROR: connect: ", err)
	}
	err = cli.Publish(path.Join(topic, "online"), 0, true, []byte("true"))
	if err != nil {
		log.Fatal("ERROR: publish: ", err)
	}
	conn, err := net.Dial("tcp", *gpsd)
	if err != nil {
		log.Fatal("ERROR: connect to gpsd: ", err)
	}
	defer conn.Close()
	_, err = fmt.Fprintln(conn, `?WATCH={"enable":true,"json":true}`)
	if err != nil {
		log.Fatal("ERROR: watch gpsd: ", err)
	}
	dec := json.NewDecoder(conn)
	var line struct {
		Class string
		Mode  int

		Time          time.Time
		Lat, Lon      float64
		EPX, EPY, EPH float64
	}
	log.Println("Connected to GPSD")
	for {
		err = dec.Decode(&line)
		if err != nil {
			log.Fatal("ERORR: invalid gpsd data: ", err)
		}
		if line.Class != "TPV" {
			continue
		}
		if line.Mode < 2 {
			continue
		}
		var pub struct {
			Time           time.Time
			Lat, Lon       float64
			LatErr, LonErr float64
			HorizErr       float64
		}
		pub.Time = line.Time
		pub.Lat, pub.Lon = line.Lat, line.Lon
		pub.LatErr, pub.LonErr = line.EPX, line.EPY
		pub.HorizErr = line.EPH
		data, err := json.Marshal(pub)
		if err != nil {
			log.Fatal("ERROR: marshal data: ", err)
		}
		err = cli.Publish(path.Join(topic, "location"), 0, true, data)
		if err != nil {
			log.Fatal("ERROR: publish: ", err)
		}
	}

}
