package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func registerPbRoutes(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	// templates
	tpl := template.Must(template.ParseFS(embeddedFS, "templates/index.html"))

	rg.GET("/", func(e *core.RequestEvent) error {
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, nil); err != nil {
			return err
		}
		return e.HTML(http.StatusOK, buf.String())
	})

	sub, _ := fs.Sub(embeddedFS, "static")
	rg.GET("/static/{path...}", func(e *core.RequestEvent) error {
		return e.FileFS(sub, e.Request.PathValue(apis.StaticWildcardParam))
	})

	rg.POST("/info", func(e *core.RequestEvent) error {
		return getInfoPB(e)
	})

	rg.POST("/download", func(e *core.RequestEvent) error {
		return startDownloadPB(e)
	})

	rg.POST("/cancel/{id}", func(e *core.RequestEvent) error {
		return cancelDownloadPB(e)
	})

	rg.GET("/progress/{id}", func(e *core.RequestEvent) error {
		return progressPB(e)
	})

	rg.GET("/download/{id}", func(e *core.RequestEvent) error {
		return serveFilePB(e)
	})
}

func progressPB(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	e.Response.Header().Set("Content-Type", "text/event-stream")
	e.Response.Header().Set("Cache-Control", "no-cache")
	e.Response.Header().Set("Connection", "keep-alive")

	var lastStage string
	flusher, _ := e.Response.(http.Flusher)

	for {
		select {
		case <-e.Request.Context().Done():
			return nil
		default:
		}

		jobsMu.RLock()
		job, ok := jobs[id]
		jobsMu.RUnlock()
		if !ok {
			e.String(http.StatusNotFound, "")
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}

		if job.Canceled {
			io.WriteString(e.Response, "event: error\ndata: descarga cancelada\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if job.Err != "" {
			fmt.Fprintf(e.Response, "event: error\ndata: %s\n\n", job.Err)
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}

		fmt.Fprintf(e.Response, "data: %d\n\n", job.Percent)
		if job.Stage != "" && job.Stage != lastStage {
			fmt.Fprintf(e.Response, "event: stage\ndata: %s\n\n", job.Stage)
			lastStage = job.Stage
		}
		if flusher != nil {
			flusher.Flush()
		}

		if job.Ready {
			fmt.Fprintf(e.Response, "event: ready\ndata: /download/%s\n\n", id)
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}

		time.Sleep(400 * time.Millisecond)
	}
}

func serveFilePB(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	jobsMu.RLock()
	job, ok := jobs[id]
	jobsMu.RUnlock()
	if !ok || job.FilePath == "" {
		return e.JSON(http.StatusNotFound, map[string]string{"error": "archivo no disponible"})
	}
	e.Response.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(job.FilePath))
	http.ServeFile(e.Response, e.Request, job.FilePath)
	return nil
}

func getInfoPB(e *core.RequestEvent) error {
	url := e.Request.FormValue("url")
	if url == "" {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": "url requerida"})
	}
	rawCookies := e.Request.FormValue("cookies")

	tmpDir, _ := os.MkdirTemp("", "ytinfo_")
	defer os.RemoveAll(tmpDir)

	cookieFile, clean, err := prepareCookieFile(rawCookies, tmpDir)
	if err != nil {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	defer clean()

	args := []string{"-J", "--no-warnings", "--skip-download"}
	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}
	args = append(args, url)

	out, err := exec.Command("yt-dlp", args...).CombinedOutput()
	if err != nil {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%v – %s", err, bytes.TrimSpace(out))})
	}

	var yt struct {
		Title      string `json:"title"`
		Thumbnail  string `json:"thumbnail"`
		Thumbnails []struct{ URL string }
		Formats    []struct {
			Vcodec, Acodec string
			Height         int
			Abr            float64
		}
		Subtitles map[string][]any `json:"subtitles"`
	}
	if err := json.Unmarshal(out, &yt); err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	vset, aset := map[int]struct{}{}, map[string]struct{}{}
	for _, f := range yt.Formats {
		if f.Vcodec != "none" && f.Height > 0 {
			vset[f.Height] = struct{}{}
		}
		if f.Acodec != "none" && f.Vcodec == "none" && f.Abr > 0 {
			aset[fmt.Sprintf("%.0f", f.Abr)] = struct{}{}
		}
	}
	videoQ := make([]int, 0, len(vset))
	for h := range vset {
		videoQ = append(videoQ, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(videoQ)))
	audioQ := make([]string, 0, len(aset))
	for a := range aset {
		audioQ = append(audioQ, a)
	}
	sort.Strings(audioQ)
	langs := make([]string, 0, len(yt.Subtitles))
	for l := range yt.Subtitles {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	thumb := yt.Thumbnail
	if len(yt.Thumbnails) > 0 {
		thumb = yt.Thumbnails[len(yt.Thumbnails)-1].URL
	}

	resp := infoResp{Title: yt.Title, ThumbURL: thumb, SubLangs: langs}
	for _, h := range videoQ {
		resp.VideoQualities = append(resp.VideoQualities, fmt.Sprintf("%d", h))
	}
	resp.AudioQualities = audioQ
	return e.JSON(http.StatusOK, resp)
}

func startDownloadPB(e *core.RequestEvent) error {
	url := e.Request.FormValue("url")
	if url == "" {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": "URL requerida"})
	}

	id := uuid.New().String()
	jobsMu.Lock()
	jobs[id] = &jobInfo{}
	jobsMu.Unlock()

	go downloadJob(
		id,
		url,
		e.Request.FormValue("cookies"),
		e.Request.FormValue("type"),
		e.Request.FormValue("quality"),
		e.Request.FormValue("sub_lang"),
	)

	return e.JSON(http.StatusOK, map[string]string{"job": id})
}

func cancelDownloadPB(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if job, ok := jobs[id]; ok {
		job.Canceled = true
		if job.Cmd != nil && job.Cmd.Process != nil {
			_ = job.Cmd.Process.Kill()
		}
		return e.JSON(http.StatusOK, map[string]string{"status": "canceled"})
	}
	return e.JSON(http.StatusNotFound, map[string]string{"error": "job no encontrado"})
}

// ginContextAdapter adapts core.RequestEvent to mimic minimal gin.Context
// used by the existing handler functions.
type ginContextAdapter struct {
	e         *core.RequestEvent
	wroteJSON bool
	status    int
	body      any
	ctx       context.Context
}

func (g *ginContextAdapter) PostForm(key string) string {
	_ = g.e.Request.ParseForm()
	return g.e.Request.FormValue(key)
}

func (g *ginContextAdapter) DefaultPostForm(key, val string) string {
	s := g.PostForm(key)
	if s == "" {
		return val
	}
	return s
}

func (g *ginContextAdapter) Param(key string) string {
	return g.e.Request.PathValue(key)
}

func (g *ginContextAdapter) String(code int, format string, values ...any) {
	g.status = code
	g.body = fmt.Sprintf(format, values...)
	g.wroteJSON = true
	g.e.String(code, g.body.(string))
}

func (g *ginContextAdapter) JSON(code int, obj any) {
	g.status = code
	g.body = obj
	g.wroteJSON = true
	_ = g.e.JSON(code, obj)
}

func (g *ginContextAdapter) Header(key, val string) {
	g.e.Response.Header().Set(key, val)
}

func (g *ginContextAdapter) Writer() http.ResponseWriter {
	return g.e.Response
}

func (g *ginContextAdapter) RequestContext() context.Context {
	if g.ctx != nil {
		return g.ctx
	}
	return g.e.Request.Context()
}
