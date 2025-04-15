package flakyhttp

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rjeczalik/notify"
)

const debug = false

type ConfigChangeAwaiter struct {
	rt          *RoundTripper
	oldRevision uint64
}

func (a *ConfigChangeAwaiter) Await(ctx context.Context) error {
	// TODO(later): obey context (trigger spurious wake-up when it expires?)
	a.rt.configMu.Lock()
	for a.rt.configRevision == a.oldRevision {
		a.rt.configCond.Wait()
	}
	a.rt.configMu.Unlock()
	return nil
}

type pair struct{ key, value string }

type configRule struct{ pairs []pair }

type RoundTripper struct {
	rulesPath  string
	fixedPairs []pair

	Underlying *http.Transport

	configMu       sync.Mutex
	configCond     *sync.Cond
	configRevision uint64
	rules          []configRule

	notifyChan chan notify.EventInfo
}

// best practice: failureMapPath should live in its own directory without any siblings (so that inotify on the directory does not get spurious events from other changes)
//
// The specified pairs allow for application-specific attributes. Each pair is
// specified in format <key>=<value>. The key can contain anything but an equals
// sign.
func NewRoundTripper(rulesPath string, pairs ...string) (*RoundTripper, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Same values as http.DefaultTransport uses:
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}
	var fixedPairs []pair
	for _, p := range pairs {
		idx := strings.IndexByte(p, '=')
		if idx == -1 {
			return nil, fmt.Errorf("malformed pair: expected format key=value")
		}
		fixedPairs = append(fixedPairs, pair{key: p[:idx], value: p[idx+1:]})
	}
	rt := &RoundTripper{
		fixedPairs: fixedPairs,
		rulesPath:  rulesPath,
		Underlying: tr,
		notifyChan: make(chan notify.EventInfo, 1),
	}
	rt.configCond = sync.NewCond(&rt.configMu)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if debug {
			log.Printf("dial(%v, %v)", network, addr)
		}
		if rt.failDial(network, addr) {
			return nil, fmt.Errorf("dial failed by flakyhttp")
		}
		return dialer.DialContext(ctx, network, addr)
	}
	if err := notify.Watch(filepath.Dir(rulesPath), rt.notifyChan, notifyEvents...); err != nil {
		return nil, err
	}
	go rt.handleConfigUpdates()
	if err := rt.loadRules(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return rt, nil
}

func (rt *RoundTripper) loadRules() error {
	b, err := ioutil.ReadFile(rt.rulesPath)
	if err != nil {
		return err
	}
	var rules []configRule
Rule:
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.Split(line, " ")
		byKey := make(map[string]string, len(parts))
		for _, p := range parts {
			idx := strings.IndexByte(p, '=')
			if idx == -1 {
				return fmt.Errorf("malformed pair: expected format key=value")
			}
			key, value := p[:idx], p[idx+1:]
			// TODO(later): validate key is known
			if key == "rate" {
				_, err := strconv.ParseInt(strings.TrimSuffix(value, "%"), 0, 64)
				if err != nil {
					return err
				}
			}
			byKey[key] = value
		}
		// Skip rule if rt.fixedPairs do not match. This comes in handy for
		// application-specific tags such as RobustIRC’s peeraddr=<peeraddr>.
		for _, p := range rt.fixedPairs {
			if value, ok := byKey[p.key]; ok && value != p.value {
				continue Rule
			}
			delete(byKey, p.key)
		}
		pairs := make([]pair, 0, len(byKey))
		for key, value := range byKey {
			pairs = append(pairs, pair{key, value})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
		rules = append(rules, configRule{pairs: pairs})
	}
	rt.configMu.Lock()
	defer rt.configMu.Unlock()
	rt.configRevision++
	rt.rules = rules
	rt.configCond.Broadcast()
	return nil
}

func (rt *RoundTripper) handleConfigUpdates() {
	for ev := range rt.notifyChan {
		if ev.Path() != rt.rulesPath {
			continue
		}
		if err := rt.loadRules(); err != nil {
			log.Printf("loading failureMap failed: %v", err)
		}
	}
}

func (rt *RoundTripper) Close() error {
	notify.Stop(rt.notifyChan)
	return nil
}

func (rt *RoundTripper) ConfigChangeAwaiter() *ConfigChangeAwaiter {
	return &ConfigChangeAwaiter{rt, rt.configRevision}
}

func (rt *RoundTripper) failCommon(stage, dest string) bool {
	rt.configMu.Lock()
	defer rt.configMu.Unlock()
Rule:
	for _, rule := range rt.rules {
		if debug {
			log.Printf("checking if rule %v matches stage=%v, dest=%v", rule, stage, dest)
		}
		for _, pair := range rule.pairs {
			switch pair.key {
			case "dest":
				if pair.value != dest {
					continue Rule
				}
			case "rate":
				// validated in loadRules; cannot fail
				rate, _ := strconv.ParseInt(strings.TrimSuffix(pair.value, "%"), 0, 64)
				rnd := rand.Float32() * 100
				if debug {
					log.Printf("rate=%v, rnd=%v", rate, rnd)
				}
				if rnd < float32(rate) {
					continue
				}
				continue Rule
			case "stage":
				if pair.value != stage {
					continue Rule
				}
			case "redial":
				if stage == "request" {
					// irrelevant for stage=dial as stage=request happens first
					rt.Underlying.CloseIdleConnections()
				}
			default:
				log.Fatalf("BUG: unexpected pair %s", pair)
			}
		}
		return true
	}
	return false
}

func (rt *RoundTripper) failDial(network, addr string) bool {
	return rt.failCommon("dial", addr)
}

func (rt *RoundTripper) failRequest(req *http.Request) bool {
	return rt.failCommon("request", req.URL.Host)
}

func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.failRequest(req) {
		return nil, fmt.Errorf("request failed by flakyhttp")
	}
	return rt.Underlying.RoundTrip(req)
}
