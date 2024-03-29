// Package robusthttp implements RobustIRC-specific logic for using HTTP as a
// transport, specifically setting the network password on all requests.
package robusthttp

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/robustirc/bridge/deadlineconn"
	"github.com/robustirc/internal/flakyhttp"
	"github.com/robustirc/rafthttp"
)

var (
	tlsCAFile = flag.String("tls_ca_file",
		"",
		"Use the specified file as trusted CA instead of the system CAs. Useful for testing.")

	rulesPath = flag.String("flakyhttp_rules_path",
		"",
		"If non-empty, a path to a flakyhttp.rules file for failure injection")
)

type robustDoer struct {
	client   http.Client
	password string
}

func (r *robustDoer) Do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth("robustirc", r.password)
	resp, err := r.client.Do(req)
	// TODO(secure): add a flag for delay for benchmarking
	return resp, err
}

// transportImpl can be overridden in custom builds where additional source
// files in this package can change clientImpl from their func init.
var transportImpl = func(deadlined bool) http.RoundTripper {
	var tlsConfig *tls.Config
	if *tlsCAFile != "" {
		roots := x509.NewCertPool()
		contents, err := ioutil.ReadFile(*tlsCAFile)
		if err != nil {
			log.Fatalf("Could not read cert.pem: %v", err)
		}
		if !roots.AppendCertsFromPEM(contents) {
			log.Fatalf("Could not parse %q, try deleting it", *tlsCAFile)
		}
		tlsConfig = &tls.Config{RootCAs: roots}
	}
	var rt http.RoundTripper
	if *rulesPath != "" {
		var err error
		var peerAddr string
		if f := flag.Lookup("peer_addr"); f != nil {
			peerAddr = f.Value.String()
		}
		rt, err = flakyhttp.NewRoundTripper(*rulesPath, "peeraddr="+peerAddr)
		if err != nil {
			log.Fatal(err)
		}
		rt.(*flakyhttp.RoundTripper).Underlying.TLSClientConfig = tlsConfig
		return rt
	}
	transport := &http.Transport{
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	if deadlined {
		// Deadline dialing and every read/write.
		transport.Dial = deadlineconn.Dialer(2*time.Second, 10*time.Second, 10*time.Second)
	} else {
		// Deadline dialing, like http.DefaultTransport.
		transport.Dial = (&net.Dialer{
			Timeout:   10 * time.Second, // http.DefaultTransport uses 30s.
			KeepAlive: 30 * time.Second,
		}).Dial
	}
	return transport
}

// Transport returns an *http.Transport respecting the *tlsCAFile flag and
// using a 10 second read/write timeout.
func Transport(deadlined bool) http.RoundTripper {
	return transportImpl(deadlined)
}

// clientImpl can be overridden in custom builds where additional source files
// in this package can change clientImpl from their func init.
var clientImpl = func(password string, deadlined bool) rafthttp.Doer {
	doer := robustDoer{
		client:   http.Client{Transport: Transport(deadlined)},
		password: password,
	}
	return &doer
}

// Client returns a net/http.Client which will set the network password
// in Do(), respects the *tlsCAFile flag and tracks the latency of requests.
func Client(password string, deadlined bool) rafthttp.Doer {
	return clientImpl(password, deadlined)
}
