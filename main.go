package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "./domain_search.config.yaml"
	defaultOutputDir  = "domains"
)

// Config structures
type Config struct {
	Version   int             `yaml:"version"`
	Generator GeneratorConfig `yaml:"generator"`
	Limits    LimitsConfig    `yaml:"limits"`
	HTTPCheck HTTPCheckConfig `yaml:"http_check"`
	Run       RunConfig       `yaml:"run"`
}

type GeneratorConfig struct {
	TLDs                 []string `yaml:"tlds"`
	MinLength            int      `yaml:"min_length"`
	MaxLength            int      `yaml:"max_length"`
	Alphabet             string   `yaml:"alphabet"`
	AllowHyphen          bool     `yaml:"allow_hyphen"`
	ForbidLeadingHyphen  bool     `yaml:"forbid_leading_hyphen"`
	ForbidTrailingHyphen bool     `yaml:"forbid_trailing_hyphen"`
	ForbidDoubleHyphen   bool     `yaml:"forbid_double_hyphen"`
}

type LimitsConfig struct {
	Concurrency   int `yaml:"concurrency"`
	RatePerSecond int `yaml:"rate_per_second"`
	MaxCandidates int `yaml:"max_candidates"`
}

type HTTPCheckConfig struct {
	Timeout         Duration `yaml:"timeout"`
	Retry           int      `yaml:"retry"`
	Method          string   `yaml:"method"`
	BodyLimit       ByteSize `yaml:"body_limit"`
	AcceptStatusMin int      `yaml:"accept_status_min"`
	AcceptStatusMax int      `yaml:"accept_status_max"`
	TryHTTPSFirst   bool     `yaml:"try_https_first"`
}

type RunConfig struct {
	Loop bool `yaml:"loop"`
}

// Duration wrapper for YAML
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	du, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = du
	return nil
}

// ByteSize wrapper for YAML values like 32KB, 2MB
type ByteSize struct{ Bytes int64 }

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	ss := strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(ss, "KB"):
		mult = 1024
		ss = strings.TrimSuffix(ss, "KB")
	case strings.HasSuffix(ss, "MB"):
		mult = 1024 * 1024
		ss = strings.TrimSuffix(ss, "MB")
	case strings.HasSuffix(ss, "GB"):
		mult = 1024 * 1024 * 1024
		ss = strings.TrimSuffix(ss, "GB")
	case strings.HasSuffix(ss, "B"):
		mult = 1
		ss = strings.TrimSuffix(ss, "B")
	default:
		// raw number of bytes
	}
	val := strings.TrimSpace(ss)
	var nBytes int64
	_, err := fmt.Sscan(val, &nBytes)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	b.Bytes = nBytes * mult
	return nil
}

// Writer that appends domains into per-TLD files under outDir (e.g., domains/ru.txt)
type TLDWriterManager struct {
	mu      sync.Mutex
	writers map[string]*bufio.Writer
	files   map[string]*os.File
	outDir  string
}

func NewTLDWriterManager(outDir string) (*TLDWriterManager, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	return &TLDWriterManager{
		writers: make(map[string]*bufio.Writer),
		files:   make(map[string]*os.File),
		outDir:  outDir,
	}, nil
}

func (m *TLDWriterManager) Write(domain string) error {
	idx := strings.LastIndexByte(domain, '.')
	if idx <= 0 || idx == len(domain)-1 {
		return nil // skip invalid
	}
	tld := domain[idx+1:]
	if tld == "" {
		return nil
	}
	path := filepath.Join(m.outDir, tld+".txt")

	m.mu.Lock()
	defer m.mu.Unlock()

	wr, ok := m.writers[path]
	if !ok {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		m.files[path] = f
		wr = bufio.NewWriterSize(f, 64*1024)
		m.writers[path] = wr
	}
	if _, err := wr.WriteString(domain + "\n"); err != nil {
		return err
	}
	// Flush immediately to ensure data is visible on disk while the program is running.
	if err := wr.Flush(); err != nil {
		return err
	}
	return nil
}

func (m *TLDWriterManager) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writers {
		_ = w.Flush()
	}
}

func (m *TLDWriterManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writers {
		_ = w.Flush()
	}
	for _, f := range m.files {
		_ = f.Close()
	}
	m.writers = map[string]*bufio.Writer{}
	m.files = map[string]*os.File{}
}

// Progress metrics and printer
type Progress struct {
	start time.Time

	enqueued int64 // candidates generated (pushed to queue)
	checked  int64 // domains processed (HTTP tried)
	found    int64 // successful domains
}

func (p *Progress) incEnqueued() { atomic.AddInt64(&p.enqueued, 1) }
func (p *Progress) incChecked()  { atomic.AddInt64(&p.checked, 1) }
func (p *Progress) incFound()    { atomic.AddInt64(&p.found, 1) }

func (p *Progress) snapshot() (enq, chk, fnd int64, elapsed time.Duration) {
	enq = atomic.LoadInt64(&p.enqueued)
	chk = atomic.LoadInt64(&p.checked)
	fnd = atomic.LoadInt64(&p.found)
	elapsed = time.Since(p.start)
	return
}

func startProgressPrinter(p *Progress, totalPlanned int64) (stop func()) {
	ticker := time.NewTicker(200 * time.Millisecond)
	done := make(chan struct{})

	render := func() {
		enq, chk, fnd, elapsed := p.snapshot()
		elapsedSec := elapsed.Seconds()
		speed := 0.0
		if elapsedSec > 0 {
			speed = float64(chk) / elapsedSec
		}
		remaining := int64(-1)
		eta := time.Duration(0)
		if totalPlanned > 0 {
			if chk >= totalPlanned {
				remaining = 0
				eta = 0
			} else {
				remaining = totalPlanned - chk
				if speed > 0 {
					eta = time.Duration(float64(remaining)/speed) * time.Second
				} else {
					eta = -1
				}
			}
		}
		eff := 0.0
		if chk > 0 {
			eff = float64(fnd) / float64(chk) * 100.0
		}
		percent := 0.0
		if totalPlanned > 0 {
			percent = math.Min(100, 100*float64(chk)/float64(totalPlanned))
		}
		bar := renderBar(percent, 30)

		elapsedStr := fmtDuration(elapsed)
		etaStr := "-"
		if eta >= 0 {
			etaStr = fmtDuration(eta)
		}
		remainStr := "N/A"
		if remaining >= 0 {
			remainStr = fmt.Sprintf("%d", remaining)
		}

		line := fmt.Sprintf("\r%s %5.1f%% | elapsed %s | ETA %s | found %d | remaining %s | speed %.1f/s | efficiency %.2f%% | generated %d | checked %d",
			bar, percent, elapsedStr, etaStr, fnd, remainStr, speed, eff, enq, chk)
		_, _ = io.WriteString(os.Stderr, line)
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				render()
			case <-done:
				render() // final
				_, _ = io.WriteString(os.Stderr, "\n")
				return
			}
		}
	}()

	return func() {
		ticker.Stop()
		close(done)
	}
}

func renderBar(percent float64, width int) string {
	if width <= 0 {
		width = 30
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	full := int(percent / 100 * float64(width))
	if full > width {
		full = width
	}
	var b strings.Builder
	b.Grow(width + 2)
	b.WriteByte('[')
	for i := 0; i < width; i++ {
		if i < full {
			b.WriteByte('#')
		} else {
			b.WriteByte('-')
		}
	}
	b.WriteByte(']')
	return b.String()
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	secs := int64(d.Seconds() + 0.5)
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	days := h / 24
	h = h % 24
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d:%02d", days, h, m, s)
	}
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func main() {
	// CLI flags
	cfgPathFlag := flag.String("config", defaultConfigPath, "path to YAML config")
	outDirFlag := flag.String("out", defaultOutputDir, "output directory for found domains")
	loopFlag := flag.Bool("loop", false, "repeat generation loop (override config.run.loop)")
	flag.Parse()

	cfg, err := loadConfig(*cfgPathFlag)
	if err != nil {
		log.Fatalf("config load error: %v", err)
	}
	if err := validateConfig(cfg); err != nil {
		log.Fatalf("config validation error: %v", err)
	}
	if *loopFlag {
		cfg.Run.Loop = *loopFlag
	}

	httpClient := &http.Client{
		Timeout: cfg.HTTPCheck.Timeout.Duration,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxConnsPerHost:     0,
			MaxIdleConnsPerHost: int(math.Max(2, float64(cfg.Limits.Concurrency/10))),
			DisableCompression:  false,
			Proxy:               nil,
			DialContext: (&net.Dialer{
				Timeout:   cfg.HTTPCheck.Timeout.Duration,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: cfg.HTTPCheck.Timeout.Duration,
			ForceAttemptHTTP2:   true,
		},
	}

	wm, err := NewTLDWriterManager(*outDirFlag)
	if err != nil {
		log.Fatalf("output directory init error: %v", err)
	}
	defer wm.Close()

	ctx := context.Background()

	totalPlanned := int64(cfg.Limits.MaxCandidates)
	if totalPlanned < 0 {
		totalPlanned = 0
	}

	log.Printf("domain CLI started (config: %s), RPS=%d, Concurrency=%d, Loop=%v, out=%s",
		*cfgPathFlag, cfg.Limits.RatePerSecond, cfg.Limits.Concurrency, cfg.Run.Loop, *outDirFlag)

	for {
		if err := runOnce(ctx, httpClient, cfg, wm, totalPlanned); err != nil {
			log.Printf("runOnce error: %v", err)
		}
		if !cfg.Run.Loop {
			break
		}
	}
	log.Printf("finished")
}

func runOnce(ctx context.Context, httpClient *http.Client, cfg Config, writer *TLDWriterManager, totalPlanned int64) error {
	candidates := make(chan string, cfg.Limits.Concurrency*2)
	wg := &sync.WaitGroup{}

	// Progress and printer
	progress := &Progress{start: time.Now()}
	stopPrinter := startProgressPrinter(progress, totalPlanned)
	defer stopPrinter()

	// Rate limiter
	rlTokens := make(chan struct{}, max(1, cfg.Limits.RatePerSecond))
	fillTokens := func() {
		for i := 0; i < cfg.Limits.RatePerSecond; i++ {
			select {
			case rlTokens <- struct{}{}:
			default:
				return
			}
		}
	}
	fillTokens()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			fillTokens()
		}
	}()

	// Workers
	worker := func() {
		defer wg.Done()
		for name := range candidates {
			// Rate limit
			select {
			case <-rlTokens:
			case <-ctx.Done():
				return
			}

			// Check domain (HTTP)
			ok, _ := checkDomain(ctx, httpClient, name, cfg.HTTPCheck)
			if ok {
				if err := writer.Write(name); err != nil {
					log.Printf("write error for %s: %v", name, err)
				} else {
					progress.incFound()
				}
			}
			progress.incChecked()
		}
	}

	nw := cfg.Limits.Concurrency
	if nw <= 0 {
		nw = 1
	}
	wg.Add(nw)
	for i := 0; i < nw; i++ {
		go worker()
	}

	// Generate candidates
	genErr := generateCandidates(cfg.Generator, func(name string) bool {
		select {
		case candidates <- name:
			progress.incEnqueued()
			// obey max candidates limit
			if cfg.Limits.MaxCandidates > 0 {
				enq, _, _, _ := progress.snapshot()
				return enq < int64(cfg.Limits.MaxCandidates)
			}
			return true
		case <-ctx.Done():
			return false
		}
	})

	close(candidates)
	wg.Wait()
	writer.Flush()

	if genErr != nil {
		return genErr
	}
	return nil
}

// checkDomain performs HTTP method to determine if a domain is "working".
func checkDomain(ctx context.Context, client *http.Client, domain string, hc HTTPCheckConfig) (bool, string) {
	method := hc.Method
	if method == "" {
		method = http.MethodGet
	}
	schemes := []string{"https", "http"}
	if !hc.TryHTTPSFirst {
		schemes = []string{"http", "https"}
	}
	bodyLimit := hc.BodyLimit.Bytes
	if bodyLimit <= 0 {
		bodyLimit = 32 * 1024
	}
	ok := false
	var finalURL string

	try := func(scheme string) bool {
		url := scheme + "://" + domain + "/"
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.CopyN(io.Discard, resp.Body, bodyLimit)
		if resp.StatusCode >= hc.AcceptStatusMin && resp.StatusCode <= hc.AcceptStatusMax {
			finalURL = url
			return true
		}
		return false
	}

	for attempt := 0; attempt <= hc.Retry; attempt++ {
		for _, scheme := range schemes {
			if try(scheme) {
				ok = true
				break
			}
		}
		if ok {
			break
		}
	}
	return ok, finalURL
}

// generateCandidates returns fully qualified domain names including TLDs.
func generateCandidates(gen GeneratorConfig, emit func(string) bool) error {
	if gen.MinLength < 1 || gen.MaxLength < gen.MinLength {
		return fmt.Errorf("invalid lengths: min=%d max=%d", gen.MinLength, gen.MaxLength)
	}
	alpha := gen.Alphabet
	if alpha == "" {
		alpha = "abcdefghijklmnopqrstuvwxyz0123456789-"
	}
	alphaRunes := []rune(alpha)
	alMap := make(map[rune]bool, len(alphaRunes))
	for _, r := range alphaRunes {
		alMap[r] = true
	}
	isAllowed := func(r rune) bool { return alMap[r] }

	for ln := gen.MinLength; ln <= gen.MaxLength; ln++ {
		// positions in alphabet
		idx := make([]int, ln)
		for {
			// Build label (without TLD)
			var b strings.Builder
			b.Grow(ln)
			valid := true
			prevHyphen := false
			for i := 0; i < ln; i++ {
				r := alphaRunes[idx[i]]
				if !isAllowed(r) {
					valid = false
					break
				}
				if r == '-' {
					if !gen.AllowHyphen ||
						(gen.ForbidLeadingHyphen && i == 0) ||
						(gen.ForbidTrailingHyphen && i == ln-1) ||
						(gen.ForbidDoubleHyphen && prevHyphen) {
						valid = false
						break
					}
					prevHyphen = true
				} else {
					prevHyphen = false
				}
				b.WriteRune(r)
			}
			if valid {
				name := b.String()
				for _, tld := range gen.TLDs {
					tld = strings.ToLower(strings.TrimSpace(tld))
					if tld == "" || tld[0] != '.' {
						continue
					}
					domain := name + tld
					if !emit(domain) {
						return nil
					}
				}
			}

			// increment odometer
			carry := 1
			for i := ln - 1; i >= 0 && carry > 0; i-- {
				idx[i] += carry
				if idx[i] >= len(alphaRunes) {
					idx[i] = 0
					carry = 1
				} else {
					carry = 0
				}
			}
			if carry > 0 {
				break
			}
		}
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// try relative clean path
		if !filepath.IsAbs(path) {
			alt := filepath.Clean(path)
			data2, err2 := os.ReadFile(alt)
			if err2 == nil {
				var c Config
				if err3 := yaml.Unmarshal(data2, &c); err3 != nil {
					return Config{}, err3
				}
				return c, nil
			}
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if len(cfg.Generator.TLDs) == 0 {
		return fmt.Errorf("generator.tlds must not be empty")
	}
	if cfg.Generator.MinLength <= 0 || cfg.Generator.MaxLength < cfg.Generator.MinLength {
		return fmt.Errorf("invalid lengths: %d..%d", cfg.Generator.MinLength, cfg.Generator.MaxLength)
	}
	if cfg.Limits.Concurrency <= 0 {
		return fmt.Errorf("limits.concurrency must be > 0")
	}
	if cfg.Limits.RatePerSecond <= 0 {
		return fmt.Errorf("limits.rate_per_second must be > 0")
	}
	if cfg.HTTPCheck.AcceptStatusMin <= 0 || cfg.HTTPCheck.AcceptStatusMax < cfg.HTTPCheck.AcceptStatusMin {
		return fmt.Errorf("invalid http_check accept status range")
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
