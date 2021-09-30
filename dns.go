package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"github.com/sunshineplan/utils"
	"github.com/sunshineplan/utils/executor"
	"github.com/sunshineplan/utils/txt"
	"golang.org/x/net/proxy"
)

var serverAddr string
var localDNSList, remoteDNSList []string

var trim = strings.TrimSpace

func formatDNSAddr(a string) string {
	hst, prt, err := net.SplitHostPort(a)
	if err != nil {
		hst = a
	}
	if trim(prt) == "" {
		prt = "53"
	}

	if ok, err := isLocal(trim(hst)); err != nil {
		log.Print(err)
		return ""
	} else if ok && prt == strconv.Itoa(*port) {
		log.Print("local dns or remote dns can not same as server dns address")
		return ""
	}

	return net.JoinHostPort(trim(hst), trim(prt))
}

func isLocal(host string) (bool, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil {
			return false, err
		} else if len(ips) == 0 {
			return false, fmt.Errorf("lookup %s: no such host", host)
		}
		ip = ips[0]
	}

	if ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.ParseIP("::1")) {
		return true, nil
	}

	return false, nil
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func process(w dns.ResponseWriter, r *dns.Msg, addr string) (err error) {
	resp, ok := getCache(r)
	if !ok {
		resp, err = dns.Exchange(r, addr)
		if err != nil {
			return
		}
		setCache(r.Question, resp)
	}

	return w.WriteMsg(resp)
}

func processProxy(w dns.ResponseWriter, r *dns.Msg, p, addr string) error {
	resp, ok := getCache(r)
	if !ok {
		u, err := url.Parse(p)
		if err != nil || u.Host == "" {
			u, err = url.Parse("http://" + p)
			if err != nil {
				return err
			}
		}
		d, err := proxy.FromURL(u, nil)
		if err != nil {
			return err
		}
		conn, err := d.Dial("tcp", addr)
		if err != nil {
			return err
		}

		c := new(dns.Client)
		resp, _, err = c.ExchangeWithConn(r, &dns.Conn{Conn: conn})
		if err != nil {
			return err
		}
		setCache(r.Question, resp)
	}

	return w.WriteMsg(resp)
}

func local(w dns.ResponseWriter, r *dns.Msg) error {
	if len(localDNSList) == 0 {
		return errors.New("no local dns provided")
	}

	if _, err := executor.ExecuteConcurrentArg(
		localDNSList,
		func(i interface{}) (_ interface{}, err error) { err = process(w, r, i.(string)); return },
	); err != nil {
		log.Print(err)
		return err
	}
	return nil
}

func remote(w dns.ResponseWriter, r *dns.Msg) (err error) {
	if len(remoteDNSList) == 0 {
		return errors.New("no remote dns provided")
	}

	if proxy := *dnsProxy; proxy != "" {
		_, err = executor.ExecuteConcurrentArg(
			remoteDNSList,
			func(i interface{}) (_ interface{}, err error) { err = processProxy(w, r, proxy, i.(string)); return },
		)
	} else {
		_, err = executor.ExecuteConcurrentArg(
			remoteDNSList,
			func(i interface{}) (_ interface{}, err error) { err = process(w, r, i.(string)); return },
		)
	}
	if err != nil {
		log.Print(err)
	}

	return
}

func loadDNSList() {
	*localDNS = trim(*localDNS)
	*remoteDNS = trim(*remoteDNS)

	if *localDNS+*remoteDNS == "" {
		log.Fatal("no dns provided")
	} else if *localDNS == "" || *remoteDNS == "" {
		log.Print("Only local dns or remote dns was provided, fallback will be enabled.")
		*fallback = true
	}

	for _, i := range splitComma(*localDNS) {
		if addr := formatDNSAddr(i); addr != "" {
			localDNSList = append(localDNSList, addr)
		}
	}

	for _, i := range splitComma(*remoteDNS) {
		if addr := formatDNSAddr(i); addr != "" {
			remoteDNSList = append(remoteDNSList, addr)
		}
	}
}

func registerHandler() {
	*list = trim(*list)
	if *list == "" {
		*list = filepath.Join(filepath.Dir(self), "remote.list")
	}
	remoteList, err := txt.ReadFile(*list)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Print("no remote list file found")
		} else {
			log.Print(err)
		}
	}

	if *fallback {
		dns.DefaultServeMux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
			executor.ExecuteSerial(
				nil,
				func(_ interface{}) (_ interface{}, err error) { err = local(w, r); return },
				func(_ interface{}) (_ interface{}, err error) { err = remote(w, r); return },
			)
		})
		if len(remoteDNSList) != 0 {
			for _, i := range remoteList {
				dns.DefaultServeMux.HandleFunc(dns.Fqdn(i), func(w dns.ResponseWriter, r *dns.Msg) {
					executor.ExecuteSerial(
						nil,
						func(_ interface{}) (_ interface{}, err error) { err = remote(w, r); return },
						func(_ interface{}) (_ interface{}, err error) { err = local(w, r); return },
					)
				})
			}
		}
	} else {
		dns.DefaultServeMux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) { local(w, r) })
		if *remoteDNS != "" {
			for _, i := range remoteList {
				dns.DefaultServeMux.HandleFunc(dns.Fqdn(i), func(w dns.ResponseWriter, r *dns.Msg) { remote(w, r) })
			}
		}
	}
}

func run() {
	var err error
	serverAddr, err = testDNSPort(*port)
	if err != nil {
		log.Fatalf("failed to test dns port: %v", err)
	}
	loadDNSList()
	parseHosts(*hosts)
	registerHandler()

	log.Printf("listen on: %s", serverAddr)
	if err := dns.ListenAndServe(serverAddr, "udp", dns.DefaultServeMux); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}

func test() error {
	*fallback = true
	addr, err := testDNSPort(0)
	if err != nil {
		return fmt.Errorf("failed to get test address: %v", err)
	}

	testHosts, err := os.CreateTemp("", "")
	if err != nil {
		return err
	}
	testHosts.WriteString("  1.2.3.4\t \tdns.test.com\t \t\n")
	testHosts.Close()
	defer os.Remove(testHosts.Name())

	ec := make(chan error)
	rc := make(chan *dns.Msg)
	done := make(chan bool)

	loadDNSList()
	parseHosts(testHosts.Name())
	registerHandler()
	go func() { ec <- dns.ListenAndServe(addr, "udp", dns.DefaultServeMux) }()

	var query = func(q, expected string) error {
		var r *dns.Msg
		m := new(dns.Msg).SetQuestion(q, dns.TypeA)
		return utils.Retry(
			func() (err error) {
				r, err = dns.Exchange(m, addr)
				if err != nil {
					return
				}
				if expected != "" {
					if result := fmt.Sprint(r.Answer); !strings.Contains(result, expected) {
						return fmt.Errorf("not expected result: %v", result)
					}
				}
				rc <- r
				return
			}, 5, 1,
		)
	}
	go func() {
		if err := query("www.google.com.", ""); err != nil {
			ec <- err
		}
		if err := query("dns.test.com.", "1.2.3.4"); err != nil {
			ec <- err
		}
		done <- true
	}()

	for {
		select {
		case err := <-ec:
			return err
		case msg := <-rc:
			if len(msg.Answer) == 0 {
				return errors.New("no result")
			}
			log.Print(msg.Answer)
		case <-done:
			log.Print("test passed")
			return nil
		}
	}
}

func testDNSPort(port int) (string, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return "", err
	}
	conn.Close()
	return conn.LocalAddr().String(), nil
}
