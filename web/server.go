package web

import (
	"bytes"
  "encoding/json"
	"fmt"
  //"html"
	"log"
	"net/http"
	"strconv"
  "strings"
	"time"

	//"golang.org/x/net/context"

	"github.com/google/zoekt"
	"github.com/google/zoekt/query"
)

const defaultNumResults = 50

type Server struct {
	Searcher zoekt.Searcher

  // Version string for this server.
	Version string

  startTime time.Time
}

func NewMux(s *Server) (*http.ServeMux, error) {
	s.startTime = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.serveSearch)
  mux.HandleFunc("/api/filetree", s.serveFileTree)

	return mux, nil
}

type FileTree struct {
  KytheUri string  `json:"kytheUri"`
  Display string   `json:"display"`
  OnlyGenerated bool  `json:"onlyGenerated"`
  Terminal bool `json:"terminal"`
  Children *[]FileTree `json:"children"`
}

func (s *Server) serveFileTree(w http.ResponseWriter, r *http.Request) {
  if err := s.serveFileTreeErr(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
}

/*
   Top-level files:  f:^[^/]*$
   That, together with repo filter, could work...
*/
func (s *Server) serveFileTreeErr(w http.ResponseWriter, r *http.Request) error {
  log.Printf("request: %v", r.URL)
  top := ""
  if tops, ok := r.URL.Query()["top"]; ok {
    top = tops[0]
  }
  topParts := strings.SplitN(top, ":", 2)
  topRepo := ""
  topPath := ""
  if len(topParts) > 0 {
    topRepo = topParts[0]
  }
  if len(topParts) > 1 {
    topPath = topParts[1]
  }

  sOpts := zoekt.SearchOptions{
		MaxWallTime: 10 * time.Second,
	}
	sOpts.SetDefaults()
  // TODO get num estimate etc

  ctx := r.Context()

  rq := "r:"
  if topRepo != "" {
    rq += topRepo
    if topPath == "" {
      // Well, zoekt obviously doesn't return dir matches. So something like
      //
      //     rq += " f:^[^/]*$"
      //
      // wouldn't work. So fetch all files from repo now, and post-process
      // to filter the relevant ones only.
      //
      // Note: we rely on getting back all files, so we can harvest the
      // top-level dirs. Need to check the num estimates above to be sure.
      rq += " f:^.*$"
    } else {
      rq += " f:^" + topPath + "/.*$"
    }
  }
  log.Printf("query: %v", rq)

  q, err := query.Parse(rq)
	if err != nil {
		return err
  }

	result, err := s.Searcher.Search(ctx, q, &sOpts)
	if err != nil {
		return err
	}

  subtrees := []FileTree{}
  if topRepo == "" {
    for r, _ := range result.RepoURLs {
      t := FileTree{
        KytheUri: r,
        Display: r,
        OnlyGenerated: false,
        Terminal: false,
        Children: nil,
      }
      subtrees = append(subtrees, t)
    }
  } else {
    seen := map[string]bool{}
    for _, f := range result.Files {
      prefix := ""
      if topPath != "" {
        prefix = topPath + "/"
      }
      relative := strings.TrimPrefix(f.FileName, prefix)
      relParts := strings.Split(relative, "/")
      currentPart := relParts[0]
      if _, exists := seen[currentPart]; !exists {
        seen[currentPart] = true
        t := FileTree{
          KytheUri: f.Repository + ":" + prefix + currentPart,
          Display: currentPart,
          OnlyGenerated: false,
          Terminal: false,
          Children: nil,
        }
        subtrees = append(subtrees, t)
      }
    }
  }

  w.Header().Set("Content-Type", "application/json; charset=UTF-8")
  w.WriteHeader(http.StatusOK)
  if err = json.NewEncoder(w).Encode(FileTree{
    KytheUri: "toplevel",
    Display: "wontshow",
    OnlyGenerated: false,
    Terminal: false,
    Children: &subtrees,
  }); err != nil {
    return err
  }
  //fmt.Fprintf(w, "{}", html.EscapeString(r.URL.Path))
  return nil
}

func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request) {
	err := s.serveSearchErr(w, r)

  // Note: zoekt-webserver checks for query suggest here. Should we?
	if err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
}

func (s *Server) serveSearchErr(w http.ResponseWriter, r *http.Request) error {
	qvals := r.URL.Query()
	queryStr := qvals.Get("q")
	if queryStr == "" {
		return fmt.Errorf("no query found")
	}

	q, err := query.Parse(queryStr)
	if err != nil {
		return err
	}

	repoOnly := true
	query.VisitAtoms(q, func(q query.Q) {
		_, ok := q.(*query.Repo)
		repoOnly = repoOnly && ok
	})
	if repoOnly {
    return fmt.Errorf("repo-only query not supported")
	}

	numStr := qvals.Get("num")

	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 {
		num = defaultNumResults
	}

	sOpts := zoekt.SearchOptions{
		MaxWallTime: 10 * time.Second,
    Whole: true,
	}

	sOpts.SetDefaults()

	ctx := r.Context()
	if result, err := s.Searcher.Search(ctx, q, &zoekt.SearchOptions{EstimateDocCount: true}); err != nil {
		return err
	} else if numdocs := result.ShardFilesConsidered; numdocs > 10000 {
		// If the search touches many shards and many files, we
		// have to limit the number of matches.  This setting
		// is based on the number of documents eligible after
		// considering reponames, so large repos (both
		// android, chromium are about 500k files) aren't
		// covered fairly.

		// 10k docs, 50 num -> max match = (250 + 250 / 10)
		sOpts.ShardMaxMatchCount = num*5 + (5*num)/(numdocs/1000)

		// 10k docs, 50 num -> max important match = 4
		sOpts.ShardMaxImportantMatch = num/20 + num/(numdocs/500)
	} else {
		// Virtually no limits for a small corpus; important
		// matches are just as expensive as normal matches.
		n := numdocs + num*100
		sOpts.ShardMaxImportantMatch = n
		sOpts.ShardMaxMatchCount = n
		sOpts.TotalMaxMatchCount = n
		sOpts.TotalMaxImportantMatch = n
	}
	sOpts.MaxDocDisplayCount = num

	result, err := s.Searcher.Search(ctx, q, &sOpts)
	if err != nil {
		return err
	}

  // TODO
	//fileMatches, err := s.formatResults(result, queryStr, s.Print)
	if err != nil {
		return err
	}

  /*
	res := ResultInput{
		Last: LastInput{
			Query:     queryStr,
			Num:       num,
			AutoFocus: true,
		},
		Stats:         result.Stats,
		Query:         q.String(),
		QueryStr:      queryStr,
		SearchOptions: sOpts.String(),
		FileMatches:   fileMatches,
	}
	if res.Stats.Wait < res.Stats.Duration/10 {
		// Suppress queueing stats if they are neglible.
		res.Stats.Wait = 0
	}

	if err := s.result.Execute(&buf, &res); err != nil {
		return err
	}
  */
	var buf bytes.Buffer
  fmt.Printf("%v", result)

	w.Write(buf.Bytes())
	return nil
}

