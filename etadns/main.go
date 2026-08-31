package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxDNSMessage     = 65535
	maxConcurrent     = 64
	maxTCPConnections = 128
)

type dnsCall struct {
	done     chan struct{}
	response []byte
	err      error
}

type dohProvider struct {
	endpoint  string
	client    *http.Client
	transport *http.Transport
}

type dnsFallback struct {
	addresses []string
	timeout   time.Duration
	next      atomic.Uint64
}

type dohForwarder struct {
	providers  []*dohProvider
	fallback   *dnsFallback
	timeout    time.Duration
	preferred  atomic.Uint32
	callsMu    sync.Mutex
	calls      map[string]*dnsCall
	logMu      sync.Mutex
	lastLog    time.Time
	suppressed uint64
}

func main() {
	listenSpec := flag.String("listen", "127.0.0.1:5353,[::1]:5353", "comma-separated local DNS listeners")
	socksAddr := flag.String("socks", "127.0.0.1:1080", "local SOCKS5 endpoint or direct")
	fwmark := flag.Uint64("fwmark", 0, "SO_MARK for direct DoH bootstrap sockets")
	endpointSpec := flag.String("endpoint", "https://dns.google/dns-query", "semicolon-separated DNS-over-HTTPS endpoints")
	bootstrapSpec := flag.String("bootstrap", "8.8.8.8:443,8.8.4.4:443", "matching semicolon-separated bootstrap address groups")
	timeout := flag.Duration("timeout", 6*time.Second, "total upstream request timeout")
	attemptTimeout := flag.Duration("attempt-timeout", 1500*time.Millisecond, "timeout for one DoH provider")
	fallbackSpec := flag.String("fallback-resolvers", "", "comma-separated numeric UDP/TCP DNS fallback addresses")
	fallbackTimeout := flag.Duration("fallback-timeout", 750*time.Millisecond, "timeout for one fallback DNS exchange")
	flag.Parse()

	listeners, err := parseList(*listenSpec, false)
	if err != nil {
		log.Fatal(err)
	}
	directBootstrap := strings.EqualFold(strings.TrimSpace(*socksAddr), "direct")
	if !directBootstrap {
		if _, _, err := net.SplitHostPort(*socksAddr); err != nil {
			log.Fatalf("invalid SOCKS address: %v", err)
		}
	}
	if *fwmark > uint64(^uint32(0)) {
		log.Fatal("fwmark must fit in 32 bits")
	}
	if *timeout < time.Second || *timeout > 2*time.Minute {
		log.Fatal("timeout must be between 1s and 2m")
	}
	if *attemptTimeout < 250*time.Millisecond || *attemptTimeout > *timeout {
		log.Fatal("attempt-timeout must be between 250ms and timeout")
	}
	providers, err := buildProviders(*endpointSpec, *bootstrapSpec, *socksAddr, uint32(*fwmark), *attemptTimeout)
	if err != nil {
		log.Fatal(err)
	}
	var fallback *dnsFallback
	if strings.TrimSpace(*fallbackSpec) != "" {
		fallbackAddresses, parseErr := parseList(*fallbackSpec, true)
		if parseErr != nil {
			log.Fatal(parseErr)
		}
		if *fallbackTimeout < 100*time.Millisecond || *fallbackTimeout > *timeout {
			log.Fatal("fallback-timeout must be between 100ms and timeout")
		}
		fallback = &dnsFallback{addresses: fallbackAddresses, timeout: *fallbackTimeout}
	}
	forwarder := &dohForwarder{
		providers: providers,
		fallback:  fallback,
		timeout:   *timeout,
		calls:     make(map[string]*dnsCall),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sem := make(chan struct{}, maxConcurrent)
	tcpSem := make(chan struct{}, maxTCPConnections)
	var servers sync.WaitGroup
	for _, address := range listeners {
		address := address
		servers.Add(2)
		go func() {
			defer servers.Done()
			if err := serveUDP(ctx, address, forwarder, sem); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("UDP %s: %v", address, err)
				cancel()
			}
		}()
		go func() {
			defer servers.Done()
			if err := serveTCP(ctx, address, forwarder, sem, tcpSem); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("TCP %s: %v", address, err)
				cancel()
			}
		}()
	}
	providerNames := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerNames = append(providerNames, provider.endpoint)
	}
	if directBootstrap {
		log.Printf("local DNS gateway listening on %s; DoH %s through direct bootstrap, fwmark %#x", strings.Join(listeners, ","), strings.Join(providerNames, ","), uint32(*fwmark))
	} else {
		log.Printf("local DNS gateway listening on %s; DoH %s through SOCKS5 %s", strings.Join(listeners, ","), strings.Join(providerNames, ","), *socksAddr)
	}
	<-ctx.Done()
	for _, provider := range providers {
		provider.transport.CloseIdleConnections()
	}
	servers.Wait()
}

func buildProviders(endpointSpec, bootstrapSpec, socksAddr string, fwmark uint32, attemptTimeout time.Duration) ([]*dohProvider, error) {
	endpointValues := strings.Split(endpointSpec, ";")
	bootstrapValues := strings.Split(bootstrapSpec, ";")
	if len(endpointValues) == 0 || len(endpointValues) != len(bootstrapValues) {
		return nil, errors.New("endpoint and bootstrap provider counts differ")
	}
	providers := make([]*dohProvider, 0, len(endpointValues))
	directBootstrap := strings.EqualFold(strings.TrimSpace(socksAddr), "direct")
	for index := range endpointValues {
		endpointValue := strings.TrimSpace(endpointValues[index])
		parsedEndpoint, err := url.Parse(endpointValue)
		if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Hostname() == "" {
			return nil, fmt.Errorf("provider %d endpoint must be an HTTPS URL with a hostname", index+1)
		}
		bootstraps, err := parseList(bootstrapValues[index], true)
		if err != nil {
			return nil, fmt.Errorf("provider %d bootstrap: %w", index+1, err)
		}
		providerBootstraps := append([]string(nil), bootstraps...)
		nextBootstrap := &atomic.Uint64{}
		dialContext := func(ctx context.Context, _, _ string) (net.Conn, error) {
			start := int(nextBootstrap.Add(1)-1) % len(providerBootstraps)
			var errs []error
			for offset := range providerBootstraps {
				target := providerBootstraps[(start+offset)%len(providerBootstraps)]
				var conn net.Conn
				var dialErr error
				if directBootstrap {
					conn, dialErr = dialMarked(ctx, target, fwmark)
				} else {
					conn, dialErr = dialSOCKS5(ctx, socksAddr, target)
				}
				if dialErr == nil {
					return conn, nil
				}
				errs = append(errs, fmt.Errorf("%s: %w", target, dialErr))
			}
			return nil, errors.Join(errs...)
		}
		transport := &http.Transport{
			Proxy:                 nil,
			DialContext:           dialContext,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   4,
			MaxConnsPerHost:       4,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   attemptTimeout,
			ResponseHeaderTimeout: attemptTimeout,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				ServerName:         parsedEndpoint.Hostname(),
				ClientSessionCache: tls.NewLRUClientSessionCache(16),
			},
		}
		providers = append(providers, &dohProvider{
			endpoint:  parsedEndpoint.String(),
			client:    &http.Client{Transport: transport, Timeout: attemptTimeout},
			transport: transport,
		})
	}
	return providers, nil
}

func parseList(spec string, numeric bool) ([]string, error) {
	var result []string
	seen := make(map[string]struct{})
	for _, value := range strings.Split(spec, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		host, portText, err := net.SplitHostPort(value)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", value, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port in %q", value)
		}
		if numeric && net.ParseIP(host) == nil {
			return nil, fmt.Errorf("bootstrap address must be numeric: %q", value)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, errors.New("address list is empty")
	}
	return result, nil
}

func (f *dohForwarder) forward(parent context.Context, query []byte) ([]byte, error) {
	if len(query) < 12 || len(query) > maxDNSMessage {
		return nil, errors.New("invalid DNS message length")
	}
	queryID := [2]byte{query[0], query[1]}
	normalized := append([]byte(nil), query...)
	normalized[0], normalized[1] = 0, 0
	key := string(normalized)

	f.callsMu.Lock()
	if call, ok := f.calls[key]; ok {
		f.callsMu.Unlock()
		select {
		case <-parent.Done():
			return nil, parent.Err()
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return responseWithID(call.response, queryID), nil
		}
	}
	call := &dnsCall{done: make(chan struct{})}
	f.calls[key] = call
	f.callsMu.Unlock()

	response, err := f.forwardOnce(parent, query)
	if err == nil {
		response = responseWithID(response, [2]byte{})
	}
	f.callsMu.Lock()
	call.response = response
	call.err = err
	delete(f.calls, key)
	close(call.done)
	f.callsMu.Unlock()
	if err != nil {
		return nil, err
	}
	return responseWithID(response, queryID), nil
}

func (f *dohForwarder) forwardOnce(parent context.Context, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, f.timeout)
	defer cancel()
	start := int(f.preferred.Load()) % len(f.providers)
	var errs []error
	for offset := range f.providers {
		index := (start + offset) % len(f.providers)
		provider := f.providers[index]
		response, err := provider.exchange(ctx, query)
		if err == nil {
			f.preferred.Store(uint32(index))
			return response, nil
		}
		provider.transport.CloseIdleConnections()
		errs = append(errs, fmt.Errorf("%s: %w", provider.endpoint, err))
		if ctx.Err() != nil {
			break
		}
	}
	if f.fallback != nil && ctx.Err() == nil {
		response, err := f.fallback.exchange(ctx, query)
		if err == nil {
			return response, nil
		}
		errs = append(errs, fmt.Errorf("fallback DNS: %w", err))
	}
	return nil, errors.Join(errs...)
}

func (p *dohProvider) exchange(ctx context.Context, query []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("Content-Type", "application/dns-message")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH status %s", resp.Status)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/dns-message") {
		return nil, fmt.Errorf("unexpected DoH content type %q", contentType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessage+1))
	if err != nil {
		return nil, err
	}
	if len(body) < 12 || len(body) > maxDNSMessage {
		return nil, errors.New("invalid DoH response length")
	}
	if err := validateDNSResponse(query, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (f *dnsFallback) exchange(ctx context.Context, query []byte) ([]byte, error) {
	start := int(f.next.Add(1)-1) % len(f.addresses)
	var errs []error
	for offset := range f.addresses {
		address := f.addresses[(start+offset)%len(f.addresses)]
		attemptCtx, cancel := context.WithTimeout(ctx, f.timeout)
		response, truncated, err := exchangeDNSUDP(attemptCtx, address, query)
		if err == nil && truncated {
			response, err = exchangeDNSTCP(attemptCtx, address, query)
		}
		cancel()
		if err == nil {
			flags := binary.BigEndian.Uint16(response[2:4])
			binary.BigEndian.PutUint16(response[2:4], flags&^0x0020)
			return response, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", address, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func exchangeDNSUDP(ctx context.Context, address string, query []byte) ([]byte, bool, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", address)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	setContextDeadline(conn, ctx)
	if err := writeAll(conn, query); err != nil {
		return nil, false, err
	}
	buffer := make([]byte, maxDNSMessage)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, false, err
	}
	response := append([]byte(nil), buffer[:n]...)
	if err := validateDNSResponse(query, response); err != nil {
		return nil, false, err
	}
	flags := binary.BigEndian.Uint16(response[2:4])
	return response, flags&0x0200 != 0, nil
}

func exchangeDNSTCP(ctx context.Context, address string, query []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	setContextDeadline(conn, ctx)
	lengthBuffer := make([]byte, 2)
	binary.BigEndian.PutUint16(lengthBuffer, uint16(len(query)))
	if err := writeAll(conn, lengthBuffer); err != nil {
		return nil, err
	}
	if err := writeAll(conn, query); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, lengthBuffer); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(lengthBuffer))
	if length < 12 {
		return nil, errors.New("short TCP DNS response")
	}
	response := make([]byte, length)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	if err := validateDNSResponse(query, response); err != nil {
		return nil, err
	}
	return response, nil
}

func setContextDeadline(conn net.Conn, ctx context.Context) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
}

func validateDNSResponse(query, response []byte) error {
	if len(query) < 12 || len(response) < 12 {
		return errors.New("short DNS message")
	}
	if query[0] != response[0] || query[1] != response[1] {
		return errors.New("DNS response ID mismatch")
	}
	queryFlags := binary.BigEndian.Uint16(query[2:4])
	responseFlags := binary.BigEndian.Uint16(response[2:4])
	if responseFlags&0x8000 == 0 {
		return errors.New("DNS response is not marked as a response")
	}
	if queryFlags&0x7800 != responseFlags&0x7800 {
		return errors.New("DNS response opcode mismatch")
	}
	if binary.BigEndian.Uint16(query[4:6]) != binary.BigEndian.Uint16(response[4:6]) {
		return errors.New("DNS response question count mismatch")
	}
	queryEnd, err := dnsQuestionEnd(query)
	if err != nil {
		return fmt.Errorf("invalid DNS query: %w", err)
	}
	responseEnd, err := dnsQuestionEnd(response)
	if err != nil {
		return fmt.Errorf("invalid DNS response question: %w", err)
	}
	if !bytes.Equal(query[12:queryEnd], response[12:responseEnd]) {
		return errors.New("DNS response question mismatch")
	}
	return nil
}

func dnsQuestionEnd(message []byte) (int, error) {
	if len(message) < 12 {
		return 0, errors.New("short header")
	}
	offset := 12
	questionCount := int(binary.BigEndian.Uint16(message[4:6]))
	for range questionCount {
		for {
			if offset >= len(message) {
				return 0, errors.New("truncated name")
			}
			labelLength := int(message[offset])
			offset++
			if labelLength == 0 {
				break
			}
			if labelLength&0xc0 == 0xc0 {
				if offset >= len(message) {
					return 0, errors.New("truncated compression pointer")
				}
				offset++
				break
			}
			if labelLength&0xc0 != 0 || labelLength > 63 || offset+labelLength > len(message) {
				return 0, errors.New("invalid label")
			}
			offset += labelLength
		}
		if offset+4 > len(message) {
			return 0, errors.New("truncated question")
		}
		offset += 4
	}
	return offset, nil
}

func responseWithID(response []byte, id [2]byte) []byte {
	copyResponse := append([]byte(nil), response...)
	if len(copyResponse) >= 2 {
		copyResponse[0], copyResponse[1] = id[0], id[1]
	}
	return copyResponse
}

func servfailResponse(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	response := append([]byte(nil), query...)
	flags := binary.BigEndian.Uint16(response[2:4])
	flags = flags&0x7910 | 0x8082
	binary.BigEndian.PutUint16(response[2:4], flags)
	binary.BigEndian.PutUint16(response[6:8], 0)
	binary.BigEndian.PutUint16(response[8:10], 0)
	return response
}

func (f *dohForwarder) logForwardError(network string, err error) {
	now := time.Now()
	f.logMu.Lock()
	if !f.lastLog.IsZero() && now.Sub(f.lastLog) < 5*time.Second {
		f.suppressed++
		f.logMu.Unlock()
		return
	}
	suppressed := f.suppressed
	f.suppressed = 0
	f.lastLog = now
	f.logMu.Unlock()
	if suppressed > 0 {
		log.Printf("DNS/%s forward: %v (%d similar errors suppressed)", network, err, suppressed)
		return
	}
	log.Printf("DNS/%s forward: %v", network, err)
}

func serveUDP(ctx context.Context, address string, forwarder *dohForwarder, sem chan struct{}) error {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	for {
		buffer := make([]byte, maxDNSMessage)
		n, peer, readErr := conn.ReadFrom(buffer)
		if readErr != nil {
			return readErr
		}
		query := append([]byte(nil), buffer[:n]...)
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				response, forwardErr := forwarder.forward(ctx, query)
				if forwardErr != nil {
					forwarder.logForwardError("UDP", forwardErr)
					response = servfailResponse(query)
				}
				if _, writeErr := conn.WriteTo(response, peer); writeErr != nil && !errors.Is(writeErr, net.ErrClosed) {
					log.Printf("DNS/UDP reply: %v", writeErr)
				}
			}()
		default:
			_, _ = conn.WriteTo(servfailResponse(query), peer)
		}
	}
}

func serveTCP(ctx context.Context, address string, forwarder *dohForwarder, sem, tcpSem chan struct{}) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return acceptErr
		}
		select {
		case tcpSem <- struct{}{}:
			go func() {
				defer func() { <-tcpSem }()
				defer conn.Close()
				serveTCPConnection(ctx, conn, forwarder, sem)
			}()
		default:
			conn.Close()
		}
	}
}

func serveTCPConnection(ctx context.Context, conn net.Conn, forwarder *dohForwarder, sem chan struct{}) {
	lengthBuffer := make([]byte, 2)
	for {
		_ = conn.SetDeadline(time.Now().Add(2 * forwarder.timeout))
		if _, err := io.ReadFull(conn, lengthBuffer); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(lengthBuffer))
		if length < 12 {
			return
		}
		query := make([]byte, length)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		var response []byte
		select {
		case sem <- struct{}{}:
			var err error
			response, err = forwarder.forward(ctx, query)
			<-sem
			if err != nil {
				forwarder.logForwardError("TCP", err)
				response = servfailResponse(query)
			}
		default:
			response = servfailResponse(query)
		}
		binary.BigEndian.PutUint16(lengthBuffer, uint16(len(response)))
		if err := writeAll(conn, lengthBuffer); err != nil {
			return
		}
		if err := writeAll(conn, response); err != nil {
			return
		}
	}
}

func dialMarked(ctx context.Context, targetAddress string, fwmark uint32) (net.Conn, error) {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	if fwmark != 0 {
		dialer.Control = func(_, _ string, raw syscall.RawConn) error {
			var socketErr error
			if err := raw.Control(func(fd uintptr) {
				socketErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, int(fwmark))
			}); err != nil {
				return err
			}
			return socketErr
		}
	}
	return dialer.DialContext(ctx, "tcp", targetAddress)
}

func dialSOCKS5(ctx context.Context, proxyAddress, targetAddress string) (net.Conn, error) {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := writeAll(conn, []byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return fail(err)
	}
	if greeting[0] != 5 || greeting[1] != 0 {
		return fail(errors.New("SOCKS5 authentication rejected"))
	}
	target, err := encodeSOCKSTarget(targetAddress)
	if err != nil {
		return fail(err)
	}
	request := append([]byte{5, 1, 0}, target...)
	if err := writeAll(conn, request); err != nil {
		return fail(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fail(fmt.Errorf("SOCKS5 connect reply %d", reply[1]))
	}
	addressLength := 0
	switch reply[3] {
	case 1:
		addressLength = net.IPv4len
	case 4:
		addressLength = net.IPv6len
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fail(err)
		}
		addressLength = int(length[0])
	default:
		return fail(errors.New("invalid SOCKS5 address type"))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressLength+2)); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func encodeSOCKSTarget(address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid target port")
	}
	result := make([]byte, 0, 1+net.IPv6len+2)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			result = append(result, 1)
			result = append(result, ip4...)
		} else {
			result = append(result, 4)
			result = append(result, ip.To16()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, errors.New("invalid target hostname")
		}
		result = append(result, 3, byte(len(host)))
		result = append(result, host...)
	}
	result = binary.BigEndian.AppendUint16(result, uint16(port))
	return result, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}
