package web

import (
	"encoding/json"
	"fmt"
	//"html"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	//"golang.org/x/net/context"

	"github.com/google/zoekt"
	"github.com/google/zoekt/query"
)

// Notes:
//
// When doing Zoekt queries, hit numbers are not estimated. This could lead to
// missing some results (though the default limits are pretty high).
//
// Some remarks about UTF-8 support in the code.

type Server struct {
	Searcher zoekt.Searcher

	// Version string for this server.
	Version string

	startTime time.Time
}

func NewMux(s *Server) (*http.ServeMux, error) {
	s.startTime = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/filetree", s.serveFileTree)
	mux.HandleFunc("/api/source", s.serveSource)
	mux.HandleFunc("/api/decor", s.serveDecors)
	mux.HandleFunc("/api/search-xref", s.serveSearchXref)

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

	subtrees := []FileTree{}
	if topRepo == "" {
		result, err := s.Searcher.List(ctx, q)
		if err != nil {
			return err
		}

		for _, re := range result.Repos {
			r := re.Repository
			if len(r.Branches) == 0 {
				// A non-git-like repo. For example plain dir.
				t := FileTree{
					KytheUri:      r.Name,
					Display:       r.Name,
					OnlyGenerated: false,
					IsFile:        false,
					Children:      nil,
				}
				subtrees = append(subtrees, t)

			} else {
				for _, b := range r.Branches {
					ticketId := r.Name + "@" + b.Name
					t := FileTree{
						KytheUri:      ticketId,
						Display:       ticketId,
						OnlyGenerated: false,
						IsFile:        false,
						Children:      nil,
					}
					subtrees = append(subtrees, t)
				}
			}
		}
	} else {
		result, err := s.Searcher.Search(ctx, q, &sOpts)
		if err != nil {
			return err
		}

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
	}
	sort.Slice(subtrees, func(i, j int) bool {
		if subtrees[i].IsFile != subtrees[j].IsFile {
			return subtrees[j].IsFile
		}
		return subtrees[i].Display < subtrees[j].Display
	})

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

// Mirrors Underhood's XRefReply.
type UhXRefReply struct {
	Refs     []UhSite `json:"refs"`
	RefCount int      `json:"refCount"`
	// Below unused by zoekt-underhood, populated to default values.
	Calls        []string `json:"calls"`
	CallCount    int      `json:"callCount"`
	Definitions  []string `json:"definitions"`
	Declarations []string `json:"declarations"`
}

type UhSite struct {
	ContainingFile UhDisplayedFile `json:"sContainingFile"`
	Snippet        UhSnippet       `json:"sSnippet"`
}

type UhDisplayedFile struct {
	FileTicket  string `json:"dfFileTicket"`
	DisplayName string `json:"dfDisplayName"`
}

type UhSnippet struct {
	Text           string  `json:"snippetText"`
	FullSpan       CmRange `json:"snippetFullSpan"`
	OccurrenceSpan CmRange `json:"snippetOccurrenceSpan"`
}

type CmRange struct {
	From CmPoint `json:"from"`
	To   CmPoint `json:"to"`
}

type CmPoint struct {
	Line int `json:"line"`
	Ch   int `json:"ch"`
}

func (s *Server) serveSearchXref(w http.ResponseWriter, r *http.Request) {
	if err := s.serveSearchXrefErr(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
	}
}

func (s *Server) serveSearchXrefErr(w http.ResponseWriter, r *http.Request) error {
	// Notes: Sources are assumed to be UTF-8 (that's what the UI expects).
	// If that wouldn't stand, either repos would need to be converted to UTF-8
	// before indexing, or we could attempt on-the-fly conversion here based on
	// heuristics.
	//
	// That said, since Zoekt API returns positions in bytes, but Underhood (and
	// CodeMirror that it uses) expects them in characters (codepoints?),
	// conversion between the two would be needed. Thankfully we would only need
	// to convert within the line, as line numbers are not affected. That could
	// be done, but in the mean time, correct line fragment spans are only
	// returned for plain-text code.
	log.Printf("request: %v", r.URL)
	selections, ok := r.URL.Query()["selection"]
	if !ok || len(selections) > 1 {
		return fmt.Errorf("expected selection parameter")
	}
	selection := selections[0]

	sOpts := zoekt.SearchOptions{
		MaxWallTime: 10 * time.Second,
	}
	sOpts.SetDefaults()
	// TODO estimate matches...

	ctx := r.Context()

	// TODO we should further massage the selection - like escape quotes.
	// Also, if we had more context in the query (repo, path), we could make
	// multiple Zoekt queries with different filters, to return more contextual
	// results with priority.
	//
	// In case no / few results are found, we could relax the filter, to be case
	// insensitive for example.
	//
	// Support for custom, pattern-based transformations and rules could be
	// added.
	rq := "\"" + selection + "\""
	log.Printf("query: %v", rq)

	q, err := query.Parse(rq)
	if err != nil {
		return err
	}

	result, err := s.Searcher.Search(ctx, q, &sOpts)
	if err != nil {
		return err
	}

	refs := []UhSite{}
	for _, f := range result.Files {
		// f.Repository, f.FileName
		ticket := f.Repository + ":" + f.FileName
		inFile := UhDisplayedFile{
			FileTicket:  ticket,
			DisplayName: ticket,
		}
		for _, l := range f.LineMatches {
			// For now we only return first fragment match in line for bolding.
			firstFrag := l.LineFragments[0]
			lineNum := l.LineNumber - 1
			snippet := UhSnippet{
				Text: string(l.Line), // TODO handle if non-UTF8 etc?
				// Inventing one based on approximation.
				FullSpan: CmRange{
					From: CmPoint{
						Line: lineNum,
						Ch:   0,
					},
					To: CmPoint{
						Line: lineNum,
						// TODO: Zoekt supplies range in bytes, while we need chars.
						//       Would need to convert based on observing line content.
						Ch: l.LineEnd - l.LineStart,
					},
				},
				OccurrenceSpan: CmRange{
					From: CmPoint{
						Line: lineNum,
						Ch:   firstFrag.LineOffset, // TODO convert from bytes to chars
					},
					To: CmPoint{
						Line: lineNum,
						Ch:   firstFrag.LineOffset + firstFrag.MatchLength, // TODO convert
					},
				},
			}
			refs = append(refs, UhSite{
				ContainingFile: inFile,
				Snippet:        snippet,
			})
		}
	}

	if err = json.NewEncoder(w).Encode(UhXRefReply{
		Refs:         refs,
		RefCount:     len(refs),
		Calls:        []string{},
		CallCount:    0,
		Definitions:  []string{},
		Declarations: []string{},
	}); err != nil {
		return err
	}
	return nil
}
