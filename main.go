package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const downloadDir = "downloads"

/* -------------------------------------------------------------------------- */
/*                           embed  de  plantillas y  JS                      */
/* -------------------------------------------------------------------------- */

//go:embed templates/index.html
//go:embed static/*
var embeddedFS embed.FS

/* -------------------------------------------------------------------------- */
/*                                 tipos / estado                             */
/* -------------------------------------------------------------------------- */

type jobInfo struct {
	Cmd      *exec.Cmd
	FilePath string
	Percent  int
	Stage    string
	Err      string
	Canceled bool
	Ready    bool
}

type infoResp struct {
	Title          string   `json:"title"`
	ThumbURL       string   `json:"thumb_url"`
	VideoQualities []string `json:"video_qualities"`
	AudioQualities []string `json:"audio_qualities"`
	SubLangs       []string `json:"sub_langs"`
}

var (
	jobs   = make(map[string]*jobInfo)
	jobsMu sync.RWMutex
)

/* --------------------------- helpers de estado ---------------------------- */

func setJobPercent(id string, p int) {
	jobsMu.Lock()
	if j, ok := jobs[id]; ok {
		j.Percent = p
	}
	jobsMu.Unlock()
}

func setJobStage(id, st string) {
	jobsMu.Lock()
	if j, ok := jobs[id]; ok && j.Stage != st {
		j.Stage = st
	}
	jobsMu.Unlock()
}

func finishJob(id, path string, err error) {
	jobsMu.Lock()
	if j, ok := jobs[id]; ok {
		if err != nil {
			j.Err = err.Error()
		}
		j.FilePath = path
		if !j.Canceled {
			j.Percent = 100
			j.Stage = "Completado ✔"
			j.Ready = true
		}
	}
	jobsMu.Unlock()
}

/* -------------------------------------------------------------------------- */
/*                                   rutas                                    */
/* -------------------------------------------------------------------------- */

func root(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", nil)
}

/* ----------------------------  /info (POST) ------------------------------- */

func getInfo(c *gin.Context) {
	url := c.PostForm("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url requerida"})
		return
	}
	cookies := c.PostForm("cookies")

	args := []string{"-J", "--no-warnings", "--skip-download"}
	if cookies != "" {
		tmp, _ := os.CreateTemp("", "ytcookies_*.json")
		_ = os.WriteFile(tmp.Name(), []byte(cookies), 0600)
		args = append(args, "--cookies", tmp.Name())
		defer os.Remove(tmp.Name())
	}
	args = append(args, url)

	/* combinedOutput → stderr + stdout */
	out, err := exec.Command("yt-dlp", args...).CombinedOutput()
	if err != nil {
		/* adjuntamos texto completo para que el frontend lo muestre */
		c.JSON(http.StatusBadRequest,
			gin.H{"error": fmt.Sprintf("%v – %s", err, bytes.TrimSpace(out))})
		return
	}

	/* … parseo JSON (sin cambios) … */
	var yt struct {
		Title      string `json:"title"`
		Thumbnail  string `json:"thumbnail"`
		Thumbnails []struct{ URL string }
		Formats    []struct {
			Vcodec string  `json:"vcodec"`
			Acodec string  `json:"acodec"`
			Height int     `json:"height"`
			Abr    float64 `json:"abr"`
		}
		Subtitles map[string][]any `json:"subtitles"`
	}
	if err := json.Unmarshal(out, &yt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	vSet, aSet := map[int]struct{}{}, map[string]struct{}{}
	for _, f := range yt.Formats {
		if f.Vcodec != "none" && f.Height > 0 {
			vSet[f.Height] = struct{}{}
		} else if f.Acodec != "none" && f.Vcodec == "none" && f.Abr > 0 {
			aSet[fmt.Sprintf("%.0f", f.Abr)] = struct{}{}
		}
	}
	videoQ := make([]int, 0, len(vSet))
	for h := range vSet {
		videoQ = append(videoQ, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(videoQ)))

	audioQ := make([]string, 0, len(aSet))
	for a := range aSet {
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
		resp.VideoQualities =
			append(resp.VideoQualities, fmt.Sprintf("%d", h))
	}
	resp.AudioQualities = audioQ

	c.JSON(http.StatusOK, resp)
}

/* ---------------------------  /download POST ------------------------------ */

func startDownload(c *gin.Context) {
	url := c.PostForm("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL requerida"})
		return
	}

	id := uuid.New().String()
	jobsMu.Lock()
	jobs[id] = &jobInfo{}
	jobsMu.Unlock()

	go downloadJob(
		id,
		url,
		c.PostForm("cookies"),
		c.DefaultPostForm("type", "video"),
		c.PostForm("quality"),
		c.PostForm("sub_lang"),
	)

	c.JSON(http.StatusOK, gin.H{"job": id})
}

/* ---------------------------  /cancel POST -------------------------------- */

func cancelDownload(c *gin.Context) {
	id := c.Param("id")
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if job, ok := jobs[id]; ok {
		job.Canceled = true
		if job.Cmd != nil && job.Cmd.Process != nil {
			_ = job.Cmd.Process.Kill()
		}
		c.JSON(http.StatusOK, gin.H{"status": "canceled"})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "job no encontrado"})
}

/* ---------------------------  /progress SSE ------------------------------ */

func progress(c *gin.Context) {
	id := c.Param("id")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	var lastStage string

	for {
		select {
		case <-c.Request.Context().Done():
			return
		default:
		}

		jobsMu.RLock()
		job, ok := jobs[id]
		jobsMu.RUnlock()
		if !ok {
			c.String(http.StatusNotFound, "")
			return
		}

		if job.Canceled {
			fmt.Fprint(c.Writer, "event: error\ndata: descarga cancelada\n\n")
			c.Writer.Flush()
			return
		}
		if job.Err != "" {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", job.Err)
			c.Writer.Flush()
			return
		}

		fmt.Fprintf(c.Writer, "data: %d\n\n", job.Percent)
		if job.Stage != "" && job.Stage != lastStage {
			fmt.Fprintf(c.Writer, "event: stage\ndata: %s\n\n", job.Stage)
			lastStage = job.Stage
		}
		c.Writer.Flush()

		if job.Ready {
			fmt.Fprintf(c.Writer, "event: ready\ndata: /download/%s\n\n", id)
			c.Writer.Flush()
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

/* ---------------------------  /download GET ------------------------------- */

func serveFile(c *gin.Context) {
	id := c.Param("id")
	jobsMu.RLock()
	job, ok := jobs[id]
	jobsMu.RUnlock()
	if !ok || job.FilePath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "archivo no disponible"})
		return
	}
	c.FileAttachment(job.FilePath, filepath.Base(job.FilePath))
}

/* -------------------------------------------------------------------------- */
/*                                    worker                                  */
/* -------------------------------------------------------------------------- */

var (
	progressRe = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)%`)
	destRe     = regexp.MustCompile(`Destination: .*\.([a-z0-9]+)`)
)

func downloadJob(id, url, cookies, media, quality, subLang string) {
	dest := filepath.Join(downloadDir, id)
	_ = os.MkdirAll(dest, 0755)

	nameTmpl := "%(title)s_%(resolution)s.%(ext)s"
	switch media {
	case "audio":
		nameTmpl = "%(title)s_audio.%(ext)s"
	case "subs":
		nameTmpl = "%(title)s_%(language)s.%(ext)s"
	case "thumb":
		nameTmpl = "%(title)s_thumb.%(ext)s"
	}
	outPath := filepath.Join(dest, nameTmpl)

	args := []string{
		"--newline",
		"--progress-template", "download:%(progress._percent_str)s",
		"-o", outPath,
	}

	switch media {
	case "audio":
		args = append(args, "-f", "bestaudio", "-x", "--audio-format", "mp3")
		setJobStage(id, "Descargando audio…")
		if quality != "" {
			args = append(args, "--audio-quality", quality)
		}
	case "subs":
		if subLang == "" {
			subLang = "en"
		}
		args = append(args, "--skip-download", "--write-sub", "--sub-lang", subLang,
			"--sub-format", "srt", "--convert-subs", "srt")
		setJobStage(id, "Descargando subtítulos…")
	case "thumb":
		args = append(args, "--skip-download", "--write-thumbnail")
		setJobStage(id, "Descargando miniatura…")
	default: // vídeo
		format := "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best"
		if quality != "" {
			format = fmt.Sprintf(
				"bestvideo[ext=mp4][height<=%s]+bestaudio[ext=m4a]"+
					"/best[ext=mp4][height<=%s]/best", quality, quality)
		}
		args = append(args, "-f", format, "--merge-output-format", "mp4")
		setJobStage(id, "Descargando vídeo…")
	}

	if cookies != "" {
		tmp := filepath.Join(dest, "cookies.json")
		_ = os.WriteFile(tmp, []byte(cookies), 0600)
		args = append(args, "--cookies", tmp)
	}
	args = append(args, url)

	cmd := exec.Command("yt-dlp", args...)
	jobsMu.Lock()
	if j, ok := jobs[id]; ok {
		j.Cmd = cmd
	}
	jobsMu.Unlock()

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		finishJob(id, "", err)
		return
	}

	go parseProgress(id, stdout)
	go parseProgress(id, stderr)

	if err := cmd.Wait(); err != nil {
		out, _ := io.ReadAll(stderr)
		finishJob(id, "", fmt.Errorf("%v – %s", err, bytes.TrimSpace(out)))
		return
	}

	/* localizar MP4 final */
	var final string
	filepath.WalkDir(dest, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && filepath.Ext(p) == ".mp4" {
			final = p
		}
		return nil
	})

	/* asegurar que no crece */
	if final != "" {
		info1, _ := os.Stat(final)
		time.Sleep(500 * time.Millisecond)
		info2, _ := os.Stat(final)
		if info1 != nil && info2 != nil && info1.Size() != info2.Size() {
			time.Sleep(500 * time.Millisecond)
		}
	}
	finishJob(id, final, nil)
}

/* ------------------------------ progreso ---------------------------------- */

func parseProgress(id string, r io.Reader) {
	rd := bufio.NewReader(r)
	for {
		line, err := rd.ReadString('\n')

		if m := destRe.FindStringSubmatch(line); len(m) == 2 {
			ext := m[1]
			switch ext {
			case "mp4", "webm":
				setJobStage(id, "Descargando vídeo…")
			case "m4a", "mp3", "opus":
				setJobStage(id, "Descargando audio…")
			}
		}
		if bytes.Contains([]byte(line), []byte("Merging")) ||
			bytes.Contains([]byte(line), []byte("ffmpeg")) {
			setJobStage(id, "Combinando (FFmpeg)…")
		}

		if m := progressRe.FindSubmatch([]byte(line)); len(m) == 2 {
			if pct, e := strconv.ParseFloat(string(m[1]), 64); e == nil {
				setJobPercent(id, int(pct))
			}
		}
		if err != nil {
			break
		}
	}
}

/* -------------------------------------------------------------------------- */
/*                                    main                                    */
/* -------------------------------------------------------------------------- */

func main() {
	r := gin.Default()

	// plantilla
	tpl := template.Must(template.ParseFS(embeddedFS, "templates/index.html"))
	r.SetHTMLTemplate(tpl)

	// static
	subFS, _ := fs.Sub(embeddedFS, "static")
	r.StaticFS("/static", http.FS(subFS))

	r.GET("/", root)
	r.POST("/info", getInfo)
	r.POST("/download", startDownload)
	r.POST("/cancel/:id", cancelDownload)
	r.GET("/progress/:id", progress)
	r.GET("/download/:id", serveFile)

	log.Println("http://localhost:8080")
	r.Run(":8080")
}
