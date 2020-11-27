// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package traceui

import (
	"html/template"
	"net/http"

	"github.com/conprof/conprof/internal/trace"
	"github.com/pkg/errors"
)

type Server struct {
	path   string
	t      trace.ParseResult
	events []*trace.Event
	ranges []Range
}

func New(t trace.ParseResult, path string) (*Server, error) {
	ranges, err := splitTrace(t)
	if err != nil {
		return nil, errors.Wrap(err, "split trace")
	}

	return &Server{
		t:      t,
		path:   path,
		ranges: ranges,
		events: t.Events,
	}, nil
}

func (s *Server) HTTPMain(w http.ResponseWriter, r *http.Request) {
	type Templ struct {
		Range []Range
		Path  string
	}

	if err := templMain.Execute(w, Templ{
		Range: s.ranges,
		Path:  s.path,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

var templMain = template.Must(template.New("").Parse(`
<html>
<body>
{{if .Range}}
	{{range $e := .Range}}
		<a href="{{$e.URL}}">View trace ({{$e.Name}})</a><br>
	{{end}}
	<br>
{{else}}
	<a href="{{.Path}}/trace">View trace</a><br>
{{end}}
<a href="{{.Path}}/goroutines">Goroutine analysis</a><br>
<a href="{{.Path}}/io">Network blocking profile</a> (<a href="{{.Path}}/io?raw=1" download="io.profile">⬇</a>)<br>
<a href="{{.Path}}/block">Synchronization blocking profile</a> (<a href="{{.Path}}/block?raw=1" download="block.profile">⬇</a>)<br>
<a href="{{.Path}}/syscall">Syscall blocking profile</a> (<a href="{{.Path}}/syscall?raw=1" download="syscall.profile">⬇</a>)<br>
<a href="{{.Path}}/sched">Scheduler latency profile</a> (<a href="{{.Path}}/sche?raw=1" download="sched.profile">⬇</a>)<br>
<a href="{{.Path}}/usertasks">User-defined tasks</a><br>
<a href="{{.Path}}/userregions">User-defined regions</a><br>
<a href="{{.Path}}/mmu">Minimum mutator utilization</a><br>
</body>
</html>
`))
