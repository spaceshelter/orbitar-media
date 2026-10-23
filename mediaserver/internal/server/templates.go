package server

import (
	"embed"
	"html/template"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var funcs = template.FuncMap{"year": func() string { return time.Now().Format("06") }, "fullYear": func() int { return time.Now().Year() }}

var (
	videoTemplate = template.Must(template.New("video.html").Funcs(funcs).ParseFS(templateFS, "templates/video.html"))
	textTemplate  = template.Must(template.New("text.html").Funcs(funcs).ParseFS(templateFS, "templates/text.html"))
)
