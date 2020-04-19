package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lestrrat-go/ical"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

var (
	port = func() string {
		port := os.Getenv("PORT")
		if port == "" {
			port = ":8080"
		} else if port[0] != ':' {
			port = ":" + port
		}
		return port
	}()
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		<-sigs
		cancel()
	}()

	// server
	NewServer(os.Args).Run(ctx)
}

type Server struct {
	// config
	url        *url.URL
	user, pass string

	// metrics
	inreqs  *prometheus.CounterVec
	outreqs prometheus.Counter

	// server
	log zerolog.Logger
	mux *http.ServeMux
	srv *http.Server
}

func NewServer(args []string) *Server {
	s := &Server{
		inreqs: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "calproxy_in_requests",
			Help: "incoming requests",
		},
			[]string{"status"},
		),
		outreqs: promauto.NewCounter(prometheus.CounterOpts{
			Name: "calproxy_outgoing_reqs",
			Help: "outgoing requests",
		}),
		log: zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, NoColor: true, TimeFormat: time.RFC3339}).With().Timestamp().Logger(),
		mux: http.NewServeMux(),
		srv: &http.Server{
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}

	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.Handle("/metrics", promhttp.Handler())
	s.mux.Handle("/", s)

	s.srv.Handler = s.mux
	s.srv.ErrorLog = log.New(s.log, "", 0)

	var ur string
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	fs.StringVar(&s.srv.Addr, "addr", port, "host:port to serve on")
	fs.StringVar(&ur, "target", os.Getenv("TARGET"), "url to redirect to")
	fs.StringVar(&s.user, "user", os.Getenv("AUTH_USER"), "user for basic auth")
	fs.StringVar(&s.pass, "pass", os.Getenv("AUTH_PASS"), "password for basic auth")
	fs.Parse(args[1:])

	var err error
	s.url, err = url.Parse(ur)
	if err != nil {
		s.log.Fatal().Err(err).Msg("parse target url")
	}
	return s
}

func (s *Server) Run(ctx context.Context) {
	errc := make(chan error)
	go func() {
		errc <- s.srv.ListenAndServe()
	}()

	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		err = s.srv.Shutdown(ctx)
	}
	s.log.Error().Err(err).Msg("server exit")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := time.Now()
	remote := r.Header.Get("x-forwarded-for")
	if remote == "" {
		remote = r.RemoteAddr
	}

	ctx := context.Background()
	cal, err := s.getAll(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.log.Error().Str("user-agent", r.Header.Get("user-agent")).Str("remote", remote).Dur("dur", time.Since(t)).Err(err).Msg("getall")
		s.inreqs.WithLabelValues("err").Inc()
		return
	}

	w.Write([]byte(cal))
	s.log.Info().Str("user-agent", r.Header.Get("user-agent")).Str("remote", remote).Dur("dur", time.Since(t)).Err(err).Msg("getall")
	s.inreqs.WithLabelValues("ok").Inc()
}

func (s *Server) getAll(ctx context.Context) (string, error) {
	urls, err := s.getIndex(ctx)
	if err != nil {
		return "", fmt.Errorf("proxy.getAll: %w", err)
	}
	cal, err := s.getIcs(ctx, urls)
	if err != nil {
		return "", fmt.Errorf("proxy.getAll: %w", err)
	}
	return cal.String(), nil
}

func (s *Server) getIndex(ctx context.Context) ([]string, error) {
	b, err := s.get(ctx, s.url.String())
	if err != nil {
		return nil, fmt.Errorf("proxy.getIndex: %w", err)
	}

	var x HTML
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

func (s *Server) getIcs(ctx context.Context, urls []string) (*ical.Calendar, error) {
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
				Scheme: s.url.Scheme,
				Host:   s.url.Host,
				Path:   u,
			}
			b, err := s.get(ctx, URL.String())
			if err != nil {
				s.log.Error().Err(err).Msg("proxy.getIcs get")
				return
			}

			cal, err := ical.NewParser().Parse(bytes.NewBuffer(b))
			if err != nil {
				s.log.Error().Err(err).Msg("proxy.getIcs parse")
			}

			for e := range cal.Entries() {
				if ev, ok := e.(*ical.Event); ok {
					icsec <- ev
				} else if tz, ok := e.(*ical.Timezone); ok {
					icstc <- tz
				} else {
					s.log.Error().Str("type", e.Type()).Msg("proxy.getIcs unhandled entry")
				}

			}
		}(u)
	}

	wg.Wait()
	done <- struct{}{}

	return <-calc, nil
}

func (s *Server) get(ctx context.Context, u string) ([]byte, error) {
	s.outreqs.Inc()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy.get build request: %w", err)
	}
	req.SetBasicAuth(s.user, s.pass)

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

type HTML struct {
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
