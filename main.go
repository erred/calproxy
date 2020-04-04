package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/lestrrat-go/ical"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"go.uber.org/zap"
)

func main() {
	port := 8080
	if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = p
	}
	var serve, target, user, pass string
	flag.IntVar(&port, "port", port, "port to serve on")
	flag.StringVar(&serve, "serve", os.Getenv("SERVE"), "url to serve on")
	flag.StringVar(&target, "target", os.Getenv("TARGET"), "url to redirect to")
	flag.StringVar(&user, "user", os.Getenv("AUTH_USER"), "user for basic auth")
	flag.StringVar(&pass, "pass", os.Getenv("AUTH_PASS"), "password for basic auth")
	// flag.StringVar(&logfmt, "logfmt", "json", "log format")
	flag.Parse()

	// logger
	prod, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stdout, "init zap: %v", err)
		os.Exit(1)
	}
	log := prod.Sugar()

	// tracer
	cfg, err := jaegercfg.FromEnv()
	if err != nil {
		log.Fatalw("init tracer env", "err", err)
	}
	cfg.ServiceName = "calproxy"
	tracer, closer, err := cfg.NewTracer()
	if err != nil {
		log.Fatalw("init tracer", "err", err)
	}
	defer closer.Close()
	opentracing.SetGlobalTracer(tracer)

	// metrics

	// http handler
	proxy, err := NewProxy(log, target, user, pass)
	if err != nil {
		log.Fatalw("init proxy", "err", err)
	}

	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	http.Handle(serve, proxy)
	http.Handle("/metrics", promhttp.Handler())
	log.Infow("starting server", "port", port, "url", serve)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

type proxy struct {
	log    *zap.SugaredLogger
	inreq  prometheus.Counter
	inok   prometheus.Counter
	inerr  prometheus.Counter
	outreq prometheus.Counter

	url *url.URL

	user, pass string
}

func NewProxy(log *zap.SugaredLogger, ur, user, pass string) (*proxy, error) {
	var err error
	p := proxy{
		log: log,

		inreq: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_reqs_total",
			Help: "The number of incoming requests",
		}),
		inok: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_reqs_ok_total",
			Help: "The number of incoming successful requests",
		}),
		inerr: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_reqs_err_total",
			Help: "The number of incoming errored requests",
		}),
		outreq: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_outgoing_reqs_total",
			Help: "The number of outgoing requests",
		}),

		user: user,
		pass: pass,
	}
	p.url, err = url.Parse(ur)
	return &p, err
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wctx, err := opentracing.GlobalTracer().Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
	if err != nil {
		p.log.Debugw("proxy.ServeHTTP extract trace header", "err", err)
	}
	span := opentracing.StartSpan("ServeHTTP", ext.RPCServerOption(wctx))
	defer span.Finish()
	ctx := opentracing.ContextWithSpan(r.Context(), span)

	p.inreq.Inc()

	t := time.Now()
	remote := r.RemoteAddr
	if fw := r.Header.Get("x-forwarded-for"); fw != "" {
		remote = fw
	}

	cal, err := p.getAll(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		p.log.Errorw("proxy.ServeHTTP getAll", "err", err, "remote", remote, "user-agent", r.Header.Get("user-agent"), "elapsed", time.Since(t))
		p.inerr.Inc()
		return
	}

	w.Write([]byte(cal))
	p.log.Infow("proxy.ServeHTTP served", "remote", remote, "user-agent", r.Header.Get("user-agent"), "elapsed", time.Since(t))
	p.inok.Inc()
}

func (p *proxy) getAll(ctx context.Context) (string, error) {
	urls, err := p.getIndex(ctx)
	if err != nil {
		return "", fmt.Errorf("proxy.getAll: %w", err)
	}
	cal, err := p.getIcs(ctx, urls)
	if err != nil {
		return "", fmt.Errorf("proxy.getAll: %w", err)
	}
	return cal.String(), nil
}

func (p *proxy) getIndex(ctx context.Context) ([]string, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "getIndex")
	defer span.Finish()

	b, err := p.get(ctx, p.url.String())
	if err != nil {
		return nil, fmt.Errorf("proxy.getIndex: %w", err)
	}

	var x Html
	err = xml.Unmarshal(b, &x)
	if err != nil {
		return nil, fmt.Errorf("proxy.getIndex unmarshal: %w", err)
	}

	var urls []string
	for _, section := range x.Body.Section {
		if section.Table.Class != "nodeTable" {
			continue
		}
	rowloop:
		for _, row := range section.Table.Tr {
			for _, cell := range row.Td {
				if cell.Class != "nameColumn" {
					continue
				}
				urls = append(urls, cell.A.Href)
				continue rowloop
			}
		}
	}
	return urls, nil
}

func (p *proxy) getIcs(ctx context.Context, urls []string) (*ical.Calendar, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "getIcs")
	defer span.Finish()

	var wg sync.WaitGroup
	icsec, icstc := make(chan *ical.Event, len(urls)), make(chan *ical.Timezone, 10)
	done, calc := make(chan struct{}), make(chan *ical.Calendar)

	cal := ical.NewCalendar()
	go func() {
	loop:
		for {
			select {
			case tz := <-icstc:
				cal.AddEntry(tz)
			case ev := <-icsec:
				cal.AddEntry(ev)
			case <-done:
				break loop
			}
		}
		close(icsec)
		close(icstc)
		calc <- cal
	}()

	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			URL := url.URL{
				Scheme: p.url.Scheme,
				Host:   p.url.Host,
				Path:   u,
			}
			b, err := p.get(ctx, URL.String())
			if err != nil {
				p.log.Errorw("proxy.getIcs get", "err", err)
				return
			}

			cal, err := ical.NewParser().Parse(bytes.NewBuffer(b))
			if err != nil {
				p.log.Errorw("proxy.getIcs parse", "err", err)
			}

			for e := range cal.Entries() {
				if ev, ok := e.(*ical.Event); ok {
					icsec <- ev
				} else if tz, ok := e.(*ical.Timezone); ok {
					icstc <- tz
				} else {
					p.log.Errorw("proxy.getIcs unhandled", "entrytype", e.Type())
				}

			}
		}(u)
	}

	wg.Wait()
	done <- struct{}{}

	return <-calc, nil
}

func (p *proxy) get(ctx context.Context, u string) ([]byte, error) {
	p.outreq.Inc()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy.get build request: %w", err)
	}
	req.SetBasicAuth(p.user, p.pass)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy.get do request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy.get resonse: %d %s", res.StatusCode, res.Status)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("proxy.get read body: %w", err)
	}
	return body, nil
}

type Html struct {
	XMLName xml.Name `xml:"html"`
	Body    struct {
		Section []struct {
			Table struct {
				Class string `xml:"class,attr"`
				Tr    []struct {
					Td []struct {
						Class string `xml:"class,attr"`
						A     struct {
							Href string `xml:"href,attr"`
						} `xml:"a"`
					} `xml:"td"`
				} `xml:"tr"`
			} `xml:"table"`
		} `xml:"section"`
	} `xml:"body"`
}
