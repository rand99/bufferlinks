//go:generate go-bindata -o bindata.go templates/...

package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/SlyMarbo/rss"
	"github.com/alexflint/bufferlinks/buffer"
	arg "github.com/alexflint/go-arg"
	_ "github.com/mattn/go-sqlite3"
	"github.com/urfave/negroni"
)

const accessToken = "1/9a1c6e4de8e136b3c04c941233350e88"

type visitor interface {
	visit(n *html.Node) visitor
}

func walkHTML(n *html.Node, v visitor) {
	vv := v.visit(n)
	if vv == nil {
		return
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walkHTML(child, vv)
	}
	v.visit(nil)
}

func attr(n *html.Node, name string) string {
	for _, at := range n.Attr {
		if strings.ToLower(at.Key) == name {
			return at.Val
		}
	}
	return ""
}

type flattenVisitor struct {
	out bytes.Buffer
}

func (v *flattenVisitor) visit(n *html.Node) visitor {
	if n == nil {
		return nil
	}
	if n.Type == html.TextNode {
		v.out.Write([]byte(n.Data + " "))
	}
	return v
}

func flatten(n *html.Node) string {
	var v flattenVisitor
	walkHTML(n, &v)
	return v.out.String()
}

type article struct {
	ID    int64
	Title string
	URL   string
	Links []*link
	Feed  string
	Date  time.Time
}

type link struct {
	ID      int64
	URL     string
	Domain  string
	Context string

	Queued   bool      // populated from DB
	QueuedAt time.Time // populated from DB
}

type byDate []*article

func (xs byDate) Len() int           { return len(xs) }
func (xs byDate) Swap(i, j int)      { xs[i], xs[j] = xs[j], xs[i] }
func (xs byDate) Less(i, j int) bool { return xs[i].Date.Before(xs[j].Date) }

type linkVisitor struct {
	parent *html.Node
	links  []*link
}

func (v *linkVisitor) visit(n *html.Node) visitor {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.A {
		if href := attr(n, "href"); href != "" {
			if url, err := url.Parse(href); err == nil {
				v.links = append(v.links, &link{
					URL:     href,
					Domain:  url.Host,
					Context: flatten(n),
				})
			}
		}
	}
	return v
}

func findLinks(s string) ([]*link, error) {
	root, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return nil, err
	}

	var v linkVisitor
	walkHTML(root, &v)
	return v.links, nil
}

func fetch(urlstr string) ([]*article, error) {
	feed, err := rss.Fetch(urlstr)
	if err != nil {
		return nil, err
	}

	feedurl, err := url.Parse(feed.Link)
	if err != nil {
		return nil, err
	}

	var all []*article
	for _, item := range feed.Items {
		if !strings.Contains(strings.ToLower(item.Title), "link") {
			continue
		}

		links, err := findLinks(item.Content)
		if err != nil {
			log.Printf("%s: %v\n", item.Title, err)
		}

		var filtered []*link
		for _, link := range links {
			linkurl, err := url.Parse(link.URL)
			if err == nil && linkurl.Host == feedurl.Host {
				//log.Println("ignoring", link.URL)
				continue
			}
			filtered = append(filtered, link)
		}

		if len(links) > 0 {
			all = append(all, &article{
				Title: item.Title,
				URL:   item.Link,
				Links: filtered,
				Feed:  feed.Title,
				Date:  item.Date,
			})
		}
	}
	return all, nil
}

func httpError(w http.ResponseWriter, format interface{}, parts ...interface{}) {
	http.Error(w, fmt.Sprintf(fmt.Sprintf("%v", format), parts...), http.StatusInternalServerError)
}

func mustParseTemplate(path string, filesystem bool) *template.Template {
	var buf []byte
	if filesystem {
		var err error
		buf, err = ioutil.ReadFile(path)
		if err != nil {
			panic(err)
		}
	} else {
		buf = MustAsset(path)
	}
	tpl, err := template.New(filepath.Base(path)).Parse(string(buf))
	if err != nil {
		panic(err)
	}
	return tpl
}

type app struct {
	store        *linkStore
	lastFetch    []*article
	bufferClient *buffer.Client
	debug        bool
	profiles     []string // IDs of buffer profiles to post to
	indexTpl     *template.Template
	enqueueTpl   *template.Template
}

func (a *app) loadTemplates() {
	log.Println("loading templates...")
	a.indexTpl = mustParseTemplate("templates/index.html", a.debug)
	a.enqueueTpl = mustParseTemplate("templates/enqueue.html", a.debug)
}

func (a *app) refreshFeeds() error {
	urlstr := "http://feeds.feedburner.com/marginalrevolution?fmt=xml"
	articles, err := fetch(urlstr)
	if err != nil {
		return err
	}
	log.Printf("parsed %d articles from %s", len(articles), urlstr)

	a.lastFetch = articles
	return nil
}

func (a *app) articles() ([]*article, error) {
	var filtered []*article
	for _, article := range a.lastFetch {
		state, err := a.store.findArticle(article.URL)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("error while looking up article from %s in DB: %v", article.URL, err)
		}
		if state != nil && !state.DismissedAt.IsZero() {
			log.Printf("%s is dismissed", article.Title)
			continue
		}

		filtered = append(filtered, article)
		for _, link := range article.Links {
			state, err := a.store.findLink(link.URL)
			if err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("error while looking up link from %s in DB: %v", article.URL, err)
			}
			if state != nil {
				link.Queued = true
				link.QueuedAt = state.QueuedAt
			}
		}
	}
	sort.Sort(byDate(filtered))
	return filtered, nil
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if a.debug {
		a.loadTemplates()
	}

	articles, err := a.articles()
	if err != nil {
		httpError(w, err)
		return
	}

	err = a.indexTpl.Execute(w, map[string]interface{}{
		"Articles": articles,
	})
	if err != nil {
		httpError(w, err)
		return
	}
}

func (a *app) handleRefresh(w http.ResponseWriter, r *http.Request) {
	err := a.refreshFeeds()
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func (a *app) handleCommit(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		httpError(w, err)
		return
	}

	content := r.FormValue("content")
	url := r.FormValue("url")
	linkTitle := r.FormValue("link_title")
	linkDescr := r.FormValue("link_descr")

	_, err = a.bufferClient.CreateUpdate(a.profiles, buffer.UpdateOptions{
		Content:         content,
		LinkURL:         url,
		LinkTitle:       linkTitle,
		LinkDescription: linkDescr,
	})
	if err != nil {
		httpError(w, err)
		return
	}

	err = a.store.markLinkQueued(url)
	if err != nil {
		httpError(w, err)
		return
	}

	fmt.Fprintln(w, "pushed post to buffer")
}

func (a *app) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if a.debug {
		a.loadTemplates()
	}

	q := r.URL.Query()
	linkurl := q.Get("url")
	if linkurl == "" {
		http.Error(w, "url not provided", http.StatusBadRequest)
		return
	}

	err := a.enqueueTpl.Execute(w, map[string]interface{}{
		"URL": linkurl,
	})
	if err != nil {
		httpError(w, err.Error())
	}
}

func main() {
	var args struct {
		Debug bool
		DB    string
	}
	args.DB = "bufferlinks.sqlite"
	arg.MustParse(&args)

	port := os.Getenv("PORT")
	if port == "" {
		port = ":19870"
	}

	// Open DB
	store, err := newLinkStore(args.DB)
	if err != nil {
		log.Fatal(err)
	}

	// Connect to Buffer
	client := buffer.NewClient(accessToken)
	profiles, err := client.Profiles()
	if err != nil {
		log.Fatal("error getting profiles:", err)
	}
	var profileIDs []string
	for _, p := range profiles {
		if p.Service == "facebook" {
			profileIDs = append(profileIDs, p.Id)
			log.Printf("using %s...", p.Service)
		}
	}

	app := app{
		store:        store,
		bufferClient: client,
		profiles:     profileIDs,
		debug:        args.Debug,
	}
	app.loadTemplates()

	go func() {
		err := app.refreshFeeds()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("fetched %d articles", len(app.lastFetch))
	}()

	// TODO: use bindata
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/enqueue", app.handleEnqueue)
	http.HandleFunc("/commit", app.handleCommit)
	http.HandleFunc("/", app.handleIndex)

	middleware := negroni.Classic()
	middleware.UseHandler(http.DefaultServeMux)

	log.Println("listening on", port)
	http.ListenAndServe(port, middleware)
}
