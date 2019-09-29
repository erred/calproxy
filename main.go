package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	var port, serve, target, user, pass string
	flag.StringVar(&port, "port", os.Getenv("PORT"), "port to serve on")
	flag.StringVar(&serve, "serve", os.Getenv("SERVE"), "url to serve on")
	flag.StringVar(&target, "target", os.Getenv("TARGET"), "url to redirect to")
	flag.StringVar(&user, "user", os.Getenv("AUTH_USER"), "user for basic auth")
	flag.StringVar(&pass, "pass", os.Getenv("AUTH_PASS"), "password for basic auth")
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
