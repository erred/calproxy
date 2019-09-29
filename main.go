package main

import (
	"flag"
	"io"
	"log"
	"net/http"
)

func main() {
	var port, serve, target, user, pass string
	flag.StringVar(&port, "port", ":80", "port to serve on")
	flag.StringVar(&serve, "serve", "/calproxy/super-secret-url", "url to serve on")
	flag.StringVar(&target, "target", "https://www.google.com", "url to redirect to")
	flag.StringVar(&user, "user", "user1", "user for basic auth")
	flag.StringVar(&pass, "pass", "hunter2", "password for basic auth")
	flag.Parse()

	http.HandleFunc(serve, func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			log.Println("build request: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req.SetBasicAuth(user, pass)

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Println("http do: ", err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	})
	log.Fatal(http.ListenAndServe(port, nil))
}
