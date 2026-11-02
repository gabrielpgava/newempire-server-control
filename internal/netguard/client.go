// Package netguard creates HTTPS clients restricted to public addresses and explicit hosts.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() { return false }
	for _, s := range []string{"0.0.0.0/8","100.64.0.0/10","192.0.0.0/24","192.0.2.0/24","198.18.0.0/15","198.51.100.0/24","203.0.113.0/24","240.0.0.0/4","2001:db8::/32","64:ff9b::/96","64:ff9b:1::/48"} {
		if netip.MustParsePrefix(s).Contains(ip) { return false }
	}
	return true
}

func New(hosts ...string) *http.Client {
	allowed := func(host string) bool {
		for _, h := range hosts { if strings.EqualFold(host,h) { return true } }
		return false
	}
	dialer := &net.Dialer{Timeout:15*time.Second,KeepAlive:30*time.Second}
	transport := &http.Transport{TLSHandshakeTimeout:15*time.Second,ResponseHeaderTimeout:30*time.Second,MaxIdleConns:16,IdleConnTimeout:60*time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn,error) {
		host, port, err := net.SplitHostPort(address); if err != nil { return nil,err }
		if port != "443" || !allowed(host) { return nil,errors.New("download host or port is not allowlisted") }
		ips, err := net.DefaultResolver.LookupNetIP(ctx,"ip",host); if err != nil { return nil,err }
		if len(ips)==0 { return nil,errors.New("download host has no addresses") }
		for _, ip := range ips { if !Public(ip) { return nil,errors.New("download host resolves to a non-public address") } }
		var last error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx,network,net.JoinHostPort(ip.String(),port)); if err == nil { return conn,nil }; last = err
		}
		return nil,last
	}
	return &http.Client{Transport:transport,Timeout:15*time.Minute,CheckRedirect:func(req *http.Request, via []*http.Request) error {
		if len(via)>=5 || req.URL.Scheme!="https" || req.URL.User!=nil || !allowed(req.URL.Hostname()) { return errors.New("unsafe download redirect") }
		return nil
	}}
}
