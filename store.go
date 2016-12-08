package main

import (
	"database/sql"
	"log"
	"os"
	"time"

	gorp "gopkg.in/gorp.v1"
)

type linkStore struct {
	sqldb *sql.DB
	db    *gorp.DbMap
}

type articleState struct {
	ID          int64
	URL         string
	DismissedAt time.Time
}

type linkState struct {
	ID         int64
	URL        string
	ArticleURL string
	QueuedAt   time.Time
}

func newLinkStore(dbpath string) (*linkStore, error) {
	db, err := sql.Open("sqlite3", dbpath)
	if err != nil {
		return nil, err
	}

	// construct a gorp DbMap
	dbmap := &gorp.DbMap{Db: db, Dialect: gorp.SqliteDialect{}}
	dbmap.TraceOn("[gorp]", log.New(os.Stdout, "[bufferlinks]", 0))

	// add a table, setting the table name to 'posts' and
	// specifying that the Id property is an auto incrementing PK
	dbmap.AddTableWithName(articleState{}, "articles").SetKeys(true, "ID")
	dbmap.AddTableWithName(linkState{}, "links").SetKeys(true, "ID")

	// create the table. in a production system you'd generally
	// use a migration tool, or create the tables via scripts
	err = dbmap.CreateTablesIfNotExists()
	if err != nil {
		return nil, err
	}

	return &linkStore{
		sqldb: db,
		db:    dbmap,
	}, nil
}

func (s *linkStore) findArticle(url string) (*articleState, error) {
	var article articleState
	err := s.db.SelectOne(&article, `SELECT * FROM articles WHERE url=? LIMIT 1`, url)
	if err != nil {
		return nil, err
	}
	return &article, nil
}

func (s *linkStore) markArticleDismissed(url string) error {
	return s.db.Insert(&articleState{
		URL:         url,
		DismissedAt: time.Now(),
	})
}

func (s *linkStore) findLink(url string) (*linkState, error) {
	var link linkState
	err := s.db.SelectOne(&link, `SELECT * FROM links WHERE url=? LIMIT 1`, url)
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (s *linkStore) markLinkQueued(url string) error {
	log.Println("inserting:", url)
	return s.db.Insert(&linkState{
		URL:      url,
		QueuedAt: time.Now(),
	})
}
