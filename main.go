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

	"github.com/lestrrat-go/ical"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	// log format, controlled by LOGFMT
	logfmt := "json"
	if os.Getenv("LOGFMT") != "json" {
		logfmt = "text"
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
	}

	// log level, controlled by LOGLVL
	level, err := zerolog.ParseLevel(os.Getenv("LOGLVL"))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}

	log.Info().Str("LOGLVL", level.String()).Str("LOGFMT", logfmt).Msg("log options")
	zerolog.SetGlobalLevel(level)
}

func main() {

	port := 8080
	if p, err := strconv.Atoi(os.Getenv("PORT")); err != nil {
		port = p
	}
	var serve, target, user, pass string
	flag.IntVar(&port, "port", port, "port to serve on")
	flag.StringVar(&serve, "serve", os.Getenv("SERVE"), "url to serve on")
	flag.StringVar(&target, "target", os.Getenv("TARGET"), "url to redirect to")
	flag.StringVar(&user, "user", os.Getenv("AUTH_USER"), "user for basic auth")
	flag.StringVar(&pass, "pass", os.Getenv("AUTH_PASS"), "password for basic auth")
	flag.Parse()

	proxy, err := NewProxy(target, user, pass)
	if err != nil {
		log.Fatal().Msgf("setup proxy: %v", err)
	}

	http.Handle(serve, proxy)
	log.Info().Int("port", port).Str("url", serve).Msg("starting server")
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

type proxy struct {
	url *url.URL
	wg  sync.WaitGroup

	user, pass string
}

func NewProxy(ur, user, pass string) (*proxy, error) {
	var err error
	p := proxy{
		user: user,
		pass: pass,
	}
	p.url, err = url.Parse(ur)
	return &p, err
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cal, err := p.getAll(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Error().Err(err).Msg("proxy.ServeHTTP")
		return
	}
	w.Write([]byte(cal))
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
	// fmt.Println(string(body))
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
				log.Error().Err(err).Msg("proxy.getIcs get")
				return
			}

			cal, err := ical.NewParser().Parse(bytes.NewBuffer(b))
			if err != nil {
				log.Error().Err(err).Msg("prox.getIcs parse")
			}

			for e := range cal.Entries() {
				if ev, ok := e.(*ical.Event); ok {
					icsec <- ev
				} else if tz, ok := e.(*ical.Timezone); ok {
					icstc <- tz
				} else {
					log.Error().Str("entrytype", e.Type()).Msg("proxy.getIcs unhandled")
				}

			}
		}(u)
	}

	wg.Wait()
	done <- struct{}{}

	return <-calc, nil
}

func (p *proxy) get(ctx context.Context, u string) ([]byte, error) {
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
	log.Debug().Int("code", res.StatusCode).Str("status", res.Status).Str("GET", u).Msg("proxy.get")
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
