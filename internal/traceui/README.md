# README

This code is copied from cmd/trace (https://golang.org/src/cmd/trace/) and refactored to make it work with non global variables.

Changes:

We've added Path template variable, which allows us to add custom prefixes to URLs linked by templated HTML.

`pprof_cmd.go` is copied from cmd/pprof (https://golang.org/src/cmd/pprof/)

`fakeflags.go` is copied from this repository, pprofui/fakeflags.go

`pprof.go` uses pprof library instead of shelling out to `go tool pprof` to make svgs.
