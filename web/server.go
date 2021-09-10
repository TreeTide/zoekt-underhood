package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	//"html"
	"log"
	"net/http"
	"sort"
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
	mux.HandleFunc("/api/source", s.serveSource)
	mux.HandleFunc("/api/decor", s.serveDecors)

	return mux, nil
}

type FileTree struct {
	// For now we use repo:path format. Name for backwards compatibility.
	// Should be unique.
	KytheUri string `json:"kytheUri"`

	// The name displayed in the tree - either a repository, or a path component.
	Display string `json:"display"`

	// Usually generated files are not indexed in Zoekt, only source.
	OnlyGenerated bool `json:"onlyGenerated"`

	// True if file, false if directory.
	IsFile bool `json:"isFile"`

	// nil means unknown, client should make a further request to discover.
	// only meaningful for directories.
	Children *[]FileTree `json:"children"`
}

func (s *Server) serveFileTree(w http.ResponseWriter, r *http.Request) {
	if err := s.serveFileTreeErr(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
}

func (s *Server) serveFileTreeErr(w http.ResponseWriter, r *http.Request) error {
	// Assumption: all paths (in request, in Zoekt response) are normalized.
	log.Printf("request: %v", r.URL)
	top := ""
	if tops, ok := r.URL.Query()["top"]; ok {
		top = tops[0]
	}
	// TODO: [ticket escaping] would be needed, in case it can contain colon.
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
		// TODO: [repo filter] in Zoekt is substring-match now, and pinning with
		//     regexp is not supported. So we must filter for the exact repo when
		//     iterating the results later.
		//
		//     But this would be better to support explicitly in Zoekt search API.
		//
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
				KytheUri:      r,
				Display:       r,
				OnlyGenerated: false,
				IsFile:        false,
				Children:      nil,
			}
			subtrees = append(subtrees, t)
		}
	} else {
		seen := map[string]bool{}
		for _, f := range result.Files {
			if f.Repository != topRepo {
				// See [repo filter]
				continue
			}
			prefix := ""
			if topPath != "" {
				prefix = topPath + "/"
			}
			relative := strings.TrimPrefix(f.FileName, prefix)
			relParts := strings.Split(relative, "/")
			currentPart := relParts[0]
			// Note: Zoekt won't return a sole directory as a match, only some files
			// within a directory. This also implies that any directory we encounter
			// will be non-empty.
			isFile := len(relParts) == 1
			if _, exists := seen[currentPart]; !exists {
				seen[currentPart] = true
				t := FileTree{
					KytheUri:      f.Repository + ":" + prefix + currentPart,
					Display:       currentPart,
					OnlyGenerated: false,
					IsFile:        isFile,
					// Note: as we query all files below 'top' now, we could as well
					// eagerly build the full subtree. That might be a future option.
					Children: nil,
				}
				subtrees = append(subtrees, t)
			}
		}
		sort.Slice(subtrees, func(i, j int) bool {
			if subtrees[i].IsFile != subtrees[j].IsFile {
				return subtrees[j].IsFile
			}
			return subtrees[i].Display < subtrees[j].Display
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	if err = json.NewEncoder(w).Encode(FileTree{
		KytheUri:      "toplevel",
		Display:       "wontshow",
		OnlyGenerated: false,
		IsFile:        false,
		Children:      &subtrees,
	}); err != nil {
		return err
	}
	//fmt.Fprintf(w, "{}", html.EscapeString(r.URL.Path))
	return nil
}

func (s *Server) serveSource(w http.ResponseWriter, r *http.Request) {
	if err := s.serveSourceErr(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
}

func (s *Server) serveSourceErr(w http.ResponseWriter, r *http.Request) error {
	log.Printf("request: %v", r.URL)
	tickets, ok := r.URL.Query()["ticket"]
	if !ok || len(tickets) > 1 {
		return fmt.Errorf("expected ticket parameter")
	}
	ticket := tickets[0]
	// See [ticket escaping]
	parts := strings.SplitN(ticket, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("Expected ticket in repo:path format")
	}
	repo := parts[0]
	path := parts[1]

	sOpts := zoekt.SearchOptions{
		MaxWallTime: 10 * time.Second,
	}
	sOpts.SetDefaults()
	// TODO estimate matches and set max counts to enable result to be included.
	//   Normally there would be exactly 1 hit, but see [repo filter] comment.
	sOpts.Whole = true

	ctx := r.Context()

	// Note the [repo filter].
	rq := "r:" + repo + " f:^" + path + "$"
	log.Printf("query: %v", rq)

	q, err := query.Parse(rq)
	if err != nil {
		return err
	}

	result, err := s.Searcher.Search(ctx, q, &sOpts)
	if err != nil {
		return err
	}

	for _, f := range result.Files {
		if f.Repository != repo {
			// See [repo filter].
			continue
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write(f.Content)
		return nil
	}
	return fmt.Errorf("Requested file not in response. Query: %v", rq)
}

// Serving decors is not supported, would need pre-calculated references.
func (s *Server) serveDecors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	// Just return an empty list of decors. string type arbitrarily chosen,
	// doesn't matter.
	if err := json.NewEncoder(w).Encode(struct {
		Decors []string `json:"decors"`
	}{
		Decors: []string{},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
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
		Whole:       true,
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