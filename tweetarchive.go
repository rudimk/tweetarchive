/**
 * Copyright 2013 Paul Smith
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"bitbucket.org/tebeka/nrsc"

	_ "github.com/bmizerany/pq"
)

type Tweet struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

const ftsSql = `
select id::text, text, ts_headline('english', text, q, 'HighlightAll=TRUE'), created_at
from tweets, plainto_tsquery('english', $1) q
where tsv @@ q order by ts_rank_cd(tsv, q) desc;
`

var db *DB

func Search(query string) (tweets []*Tweet, e error) {
	rows, err := db.conn.Query(ftsSql, query)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		tweet := &Tweet{}
		var headline string
		err = rows.Scan(&tweet.ID, &tweet.Text, &headline, &tweet.Timestamp)
		if err != nil {
			return nil, err
		}
		tweets = append(tweets, tweet)
	}
	return tweets, nil
}

func SearchHandler(w http.ResponseWriter, r *http.Request) {
	var (
		tweets []*Tweet
		err    error
	)
	q := r.FormValue("q")
	if q != "" {
		log.Print(q)
		tweets, err = Search(q)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(struct {
		Tweets []*Tweet `json:"tweets"`
	}{tweets})
	if err != nil {
		log.Println("couldn't marshal JSON search results", err)
	}
	w.Write(b)
}

type Archive struct {
	Reader *zip.Reader
}

func NewArchive(r io.Reader) (*Archive, error) {
	var b bytes.Buffer
	_, err := io.Copy(&b, r)
	if err != nil {
		return nil, err
	}
	brdr := bytes.NewReader(b.Bytes())
	zrdr, err := zip.NewReader(brdr, int64(brdr.Len()))
	if err != nil {
		return nil, err
	}
	return &Archive{zrdr}, nil
}

const tweetJsonGlob = `data/js/tweets/????_??.js`

// Tests if this is a valid tweet archive, as it looked downloaded from Twitter
func (a *Archive) Valid() bool {
	paths := make(map[string]bool)
	for _, f := range a.Reader.File {
		paths[f.Name] = true
	}
	expected := []string{
		"data/js/tweet_index.js",
		"data/js/user_details.js",
		"data/js/payload_details.js",
	}
	for _, path := range expected {
		if !paths[path] {
			log.Printf("expected %s in zip file", path)
			return false
		}
	}
	foundTweets := false
	for path, _ := range paths {
		if matched, _ := filepath.Match(tweetJsonGlob, path); matched {
			foundTweets = true
			break
		}
	}
	if !foundTweets {
		log.Printf("expected to find at least one tweets JSON file in zip archive")
		return false
	}
	return true
}

type DB struct {
	conn *sql.DB
}

func newDb(name, host string, port int) (*DB, error) {
	connStr := fmt.Sprintf("dbname=%s host=%s port=%d sslmode=disable", name, host, port)
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	return &DB{conn}, nil
}

func (d *DB) tableExists() bool {
	row := d.conn.QueryRow("select true from pg_tables where tablename = 'tweets'")
	var found bool
	if err := row.Scan(&found); err != nil {
		return false
	} else {
		return true
	}
	panic("unreachable")
}

func (d *DB) createTable() error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Commit()
	_, err = tx.Exec(createSql)
	if err != nil {
		return err
	}
	return nil
}

func (d *DB) insertTweets(tweets []interface{}) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Commit()
	stmt, err := d.conn.Prepare(insertSql)
	if err != nil {
		return err
	}
	for _, it := range tweets {
		t := it.(map[string]interface{})
		id, err := strconv.ParseInt(t["id_str"].(string), 10, 64)
		if err != nil {
			return err
		}
		_, err = stmt.Exec(
			id,
			t["created_at"].(string),
			nil,
			t["text"].(string),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

const insertSql = `insert into tweets (id, created_at, geog, text) values ($1, $2, $3, $4)`

func UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		// Check uploaded archive zipfile is valid
		f, _, err := r.FormFile("zipfile")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		archive, err := NewArchive(f)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !archive.Valid() {
			http.Error(w, "invalid tweet archive zipfile", 500)
			return
		}
		for _, f := range archive.Reader.File {
			if matched, _ := filepath.Match(tweetJsonGlob, f.Name); !matched {
				continue
			}
			rc, err := f.Open()
			defer rc.Close()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Discard first line
			var bb bytes.Buffer
			io.Copy(&bb, rc)
			b := make([]byte, bb.Len())
			bb.Read(b)
			index := bytes.Index(b, []byte("\n"))
			var tweets interface{}
			err = json.Unmarshal(b[index:len(b)], &tweets)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			err = db.insertTweets(tweets.([]interface{}))
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		http.Redirect(w, r, "/", 302)
	}
	w.Write(uploadHtml)
}

var indexHtml, uploadHtml []byte

var dbname = flag.String("dbname", "tweetarchive", "database name")
var dbhost = flag.String("dbhost", "localhost", "database host")
var dbport = flag.Int("dbport", 5432, "database port")
var port = flag.Int("port", 13331, "web server port")

func loadTemplate(name string, tvar *[]byte) {
	rdr, err := nrsc.Get(name).Open()
	if err != nil {
		panic(err)
	}
	*tvar, err = ioutil.ReadAll(rdr)
	if err != nil {
		panic(err)
	}
}

func init() {
	flag.Parse()

	var err error
	db, err = newDb(*dbname, *dbhost, *dbport)
	if err != nil {
		fmt.Fprintln(os.Stderr, "couldn't connect to the database:", err)
		os.Exit(1)
	}
	if !db.tableExists() {
		log.Println("creating tweets table")
		if err := db.createTable(); err != nil {
			fmt.Fprintln(os.Stderr, "couldn't create the tweets table:", err)
			os.Exit(1)
		}
	}

	nrsc.Initialize()
	loadTemplate("index.html", &indexHtml)
	loadTemplate("upload.html", &uploadHtml)
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(indexHtml)
	})
	http.HandleFunc("/search", SearchHandler)
	http.HandleFunc("/upload", UploadHandler)
	nrsc.Handle("/static/")
	http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
}

const createSql = `
create table tweets (
	id bigint,
	created_at timestamp,
	geog geography(point),
	text text,
	is_reply boolean default 'f',
	is_rt boolean default 'f',
	in_reply_to_status_id bigint,
	hashtags text[],
	user_mentions text[],
	tsv tsvector,
	full_tweet json,
	primary key (id)
);

create trigger ts_tsv before insert or update on tweets for each row execute procedure tsvector_update_trigger(tsv, 'pg_catalog.english', text);

create index on tweets using gin(tsv);

create index on tweets using gist(geog);
`
