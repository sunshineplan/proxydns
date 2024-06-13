package main

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/sunshineplan/utils/cache"
)

var dnsCache = cache.New[string, *dns.Msg](true)

func getCache(r *dns.Msg) (*dns.Msg, bool) {
	return dnsCache.Get(fmt.Sprint(r.Question))
}

func setCache(key []dns.Question, r *dns.Msg) {
	if len(r.Answer) == 0 {
		dnsCache.Set(fmt.Sprint(key), r, 300*time.Second, nil)
		return
	}

	ttl := r.Answer[0].Header().Ttl
	if ttl == 0 {
		ttl = 300
	}
	dnsCache.Set(fmt.Sprint(key), r, time.Duration(ttl)*time.Second, nil)
}
