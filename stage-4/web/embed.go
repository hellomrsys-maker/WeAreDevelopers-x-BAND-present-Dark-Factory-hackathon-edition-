package web

import _ "embed"

//go:embed index.html
var IndexHTML []byte

//go:embed showcase_narration.wav
var ShowcaseNarrationWAV []byte
