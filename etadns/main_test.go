package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestParseList(t *testing.T) {
	got, err := parseList("8.8.8.8:443, 8.8.4.4:443,8.8.8.8:443", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"8.8.8.8:443", "8.8.4.4:443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseList() = %v, want %v", got, want)
	}
}

func TestParseListRejectsHostnameBootstrap(t *testing.T) {
	if _, err := parseList("dns.google:443", true); err == nil {
		t.Fatal("parseList accepted a non-numeric bootstrap")
	}
}

func TestEncodeSOCKSTargetIPv4(t *testing.T) {
	got, err := encodeSOCKSTarget("8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 8, 8, 8, 8, 1, 187}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encodeSOCKSTarget() = %v, want %v", got, want)
	}
}

func TestEncodeSOCKSTargetIPv6(t *testing.T) {
	got, err := encodeSOCKSTarget("[2001:4860:4860::8888]:443")
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{4}, net.ParseIP("2001:4860:4860::8888").To16()...)
	want = append(want, 1, 187)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encodeSOCKSTarget() = %v, want %v", got, want)
	}
}

func TestResponseWithIDDoesNotMutateSource(t *testing.T) {
	source := []byte{0, 0, 0x81, 0x80}
	got := responseWithID(source, [2]byte{0x12, 0x34})
	if got[0] != 0x12 || got[1] != 0x34 {
		t.Fatalf("response ID = %x, want 1234", got[:2])
	}
	if source[0] != 0 || source[1] != 0 {
		t.Fatal("responseWithID mutated its source")
	}
}

func TestServfailResponse(t *testing.T) {
	query := []byte{
		0x12, 0x34, 0x01, 0x10,
		0, 1, 0, 2, 0, 3, 0, 0,
		3, 'w', 'w', 'w', 0, 0, 1, 0, 1,
	}
	got := servfailResponse(query)
	if got[0] != 0x12 || got[1] != 0x34 {
		t.Fatalf("response ID = %x, want 1234", got[:2])
	}
	flags := binary.BigEndian.Uint16(got[2:4])
	if flags&0x8000 == 0 || flags&0x000f != 2 {
		t.Fatalf("response flags = %#04x, want QR and SERVFAIL", flags)
	}
	if binary.BigEndian.Uint16(got[6:8]) != 0 || binary.BigEndian.Uint16(got[8:10]) != 0 {
		t.Fatal("SERVFAIL retained answer or authority counts")
	}
	if binary.BigEndian.Uint16(got[4:6]) != 1 {
		t.Fatal("SERVFAIL did not retain question count")
	}
}

func TestValidateDNSResponse(t *testing.T) {
	query := []byte{
		0x12, 0x34, 0x01, 0x00,
		0, 1, 0, 0, 0, 0, 0, 0,
		3, 'w', 'w', 'w', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0,
		0, 1, 0, 1,
	}
	response := servfailResponse(query)
	if err := validateDNSResponse(query, response); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	response[1]++
	if err := validateDNSResponse(query, response); err == nil {
		t.Fatal("response with mismatched ID accepted")
	}
}

func TestBuildProviders(t *testing.T) {
	providers, err := buildProviders(
		"https://one.example/dns-query;https://two.example/dns-query",
		"192.0.2.1:443;192.0.2.2:443",
		"127.0.0.1:1080",
		0,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, provider := range providers {
			provider.transport.CloseIdleConnections()
		}
	}()
	if len(providers) != 2 {
		t.Fatalf("provider count = %d, want 2", len(providers))
	}
	if _, err := buildProviders(
		"https://one.example/dns-query;https://two.example/dns-query",
		"192.0.2.1:443",
		"127.0.0.1:1080",
		0,
		time.Second,
	); err == nil {
		t.Fatal("mismatched provider groups accepted")
	}
	directProviders, err := buildProviders(
		"https://one.example/dns-query",
		"192.0.2.1:443",
		"direct",
		0x10000000,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	directProviders[0].transport.CloseIdleConnections()
}

func TestDoHProviderFailover(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/dns-message")
		_, _ = writer.Write(servfailResponse(query))
	}))
	defer server.Close()
	serverTransport := server.Client().Transport.(*http.Transport)
	forwarder := &dohForwarder{
		providers: []*dohProvider{
			{
				endpoint: "https://failed.example/dns-query",
				client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("provider unavailable")
				})},
				transport: &http.Transport{},
			},
			{
				endpoint:  server.URL,
				client:    server.Client(),
				transport: serverTransport,
			},
		},
		timeout: 3 * time.Second,
		calls:   make(map[string]*dnsCall),
	}
	query := []byte{
		0xab, 0xcd, 0x01, 0x00,
		0, 1, 0, 0, 0, 0, 0, 0,
		3, 'w', 'w', 'w', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0,
		0, 1, 0, 1,
	}
	response, err := forwarder.forward(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDNSResponse(query, response); err != nil {
		t.Fatal(err)
	}
	if got := forwarder.preferred.Load(); got != 1 {
		t.Fatalf("preferred provider = %d, want 1", got)
	}
}

func TestDNSUDPFallbackExchange(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	query := []byte{
		0x45, 0x67, 0x01, 0x00,
		0, 1, 0, 0, 0, 0, 0, 0,
		3, 'w', 'w', 'w', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0,
		0, 1, 0, 1,
	}
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, maxDNSMessage)
		n, peer, readErr := listener.ReadFrom(buffer)
		if readErr != nil {
			done <- readErr
			return
		}
		_, writeErr := listener.WriteTo(servfailResponse(buffer[:n]), peer)
		done <- writeErr
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response, truncated, err := exchangeDNSUDP(ctx, listener.LocalAddr().String(), query)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("response unexpectedly marked truncated")
	}
	if err := validateDNSResponse(query, response); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
