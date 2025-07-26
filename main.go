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
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const downloadDir = "downloads"

/* -------------------------------------------------------------------------- */
/*                    archivos embebidos (HTML + JS + CSS)                    */
/* -------------------------------------------------------------------------- */

//go:embed templates/index.html
//go:embed static/*
var embeddedFS embed.FS

/* -------------------------------------------------------------------------- */
/*                              tipos y estado                                */
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

/* -------------------------------------------------------------------------- */
/*                              helpers de estado                             */
/* -------------------------------------------------------------------------- */

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
/*                   cookies: JSON → Netscape conversión                      */
/* -------------------------------------------------------------------------- */

type chromeCookie struct {
	Domain string  `json:"domain"`
	Name   string  `json:"name"`
	Value  string  `json:"value"`
	Path   string  `json:"path"`
	Secure bool    `json:"secure"`
	Expiry float64 `json:"expirationDate"`
}

func jsonToNetscape(js string) (string, error) {
	var arr []chromeCookie
	if err := json.Unmarshal([]byte(js), &arr); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	for _, c := range arr {
		hostOnly := "FALSE"
		if strings.HasPrefix(c.Domain, ".") {
			hostOnly = "TRUE"
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		exp := int64(c.Expiry + 0.5) // redondeo
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			c.Domain, hostOnly, c.Path, secure, exp, c.Name, c.Value)
	}
	return b.String(), nil
}

/* crea archivo tmp con cookies (json o netscape) y devuelve ruta + cleanup */
func prepareCookieFile(raw string, workDir string) (string, func(), error) {
	if raw == "" {
		return "", func() {}, nil
	}
	// heurística: JSON empieza con '['
	var txt string
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var err error
		txt, err = jsonToNetscape(raw)
		if err != nil {
			return "", nil, err
		}
	} else {
		txt = raw
	}
	tmp := filepath.Join(workDir, "cookies.txt")
	if err := os.WriteFile(tmp, []byte(txt), 0600); err != nil {
		return "", nil, err
	}
	return tmp, func() { os.Remove(tmp) }, nil
}

/* -------------------------------------------------------------------------- */
/*                                  rutas                                     */
/* -------------------------------------------------------------------------- */

func root(c *gin.Context) { c.HTML(http.StatusOK, "index.html", nil) }

/* --------------------------- /info  POST ---------------------------------- */

func getInfoGin(c *gin.Context) {
	url := c.PostForm("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url requerida"})
		return
	}
	rawCookies := c.PostForm("cookies")

	tmpDir, _ := os.MkdirTemp("", "ytinfo_")
	defer os.RemoveAll(tmpDir)

	cookieFile, clean, err := prepareCookieFile(rawCookies, tmpDir)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer clean()

	args := []string{"-J", "--no-warnings", "--skip-download"}
	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}
	args = append(args, url)

	out, err := exec.Command("yt-dlp", args...).CombinedOutput()
	if err != nil {
		c.JSON(http.StatusBadRequest,
			gin.H{"error": fmt.Sprintf("%v – %s", err, bytes.TrimSpace(out))})
		return
	}

	/* parse JSON de yt-dlp (igual que antes) */
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
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
	c.JSON(http.StatusOK, resp)
}

/* --------------------------- /download POST ------------------------------ */

func startDownloadGin(c *gin.Context) {
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
		c.PostForm("cookies"), // ← único campo
		c.DefaultPostForm("type", "video"),
		c.PostForm("quality"),
		c.PostForm("sub_lang"),
	)

	c.JSON(http.StatusOK, gin.H{"job": id})
}

/* ---------------------------  /cancel POST -------------------------------- */

func cancelDownloadGin(c *gin.Context) {
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

func progressGin(c *gin.Context) {
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

func serveFileGin(c *gin.Context) {
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

func downloadJob(id, url, rawCookies, media, quality, subLang string) {
	/* -------- carpeta de trabajo -------- */
	dest := filepath.Join(downloadDir, id)
	_ = os.MkdirAll(dest, 0755)

	/* nombre de salida legible */
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

	/* argumentos base */
	args := []string{
		"--newline",
		"--progress-template", "download:%(progress._percent_str)s",
		"-o", outPath,
	}

	/* flags según tipo */
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
		args = append(args,
			"--skip-download", "--write-sub",
			"--sub-lang", subLang, "--sub-format", "srt", "--convert-subs", "srt")
		setJobStage(id, "Descargando subtítulos…")

	case "thumb":
		args = append(args, "--skip-download", "--write-thumbnail")
		setJobStage(id, "Descargando miniatura…")

	default: // video
		format := "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best"
		if quality != "" {
			format = fmt.Sprintf(
				"bestvideo[ext=mp4][height<=%s]+bestaudio[ext=m4a]"+
					"/best[ext=mp4][height<=%s]/best", quality, quality)
		}
		args = append(args, "-f", format, "--merge-output-format", "mp4")
		setJobStage(id, "Descargando video…")
	}

	/* ---------- cookies (JSON o Netscape) ---------- */
	if rawCookies != "" {
		cookieFile, _, err := prepareCookieFile(rawCookies, dest)
		if err != nil {
			finishJob(id, "", fmt.Errorf("cookies: %v", err))
			return
		}
		args = append(args, "--cookies", cookieFile)
	}

	args = append(args, url)

	/* lanzar yt-dlp */
	cmd := exec.Command("yt-dlp", args...)

	// guardar cmd para poder cancelar
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
		buf, _ := io.ReadAll(stderr)
		finishJob(id, "", fmt.Errorf("%v – %s", err, bytes.TrimSpace(buf)))
		return
	}

	/* localizar el .mp4 final */
	var final string
	filepath.WalkDir(dest, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && filepath.Ext(p) == ".mp4" {
			final = p
		}
		return nil
	})

	/* asegurarse de que ya no crece */
	if final != "" {
		s1, _ := os.Stat(final)
		time.Sleep(500 * time.Millisecond)
		s2, _ := os.Stat(final)
		if s1 != nil && s2 != nil && s1.Size() != s2.Size() {
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
				setJobStage(id, "Descargando video…")
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

	// template
	tpl := template.Must(template.ParseFS(embeddedFS, "templates/index.html"))
	r.SetHTMLTemplate(tpl)

	// static
	sub, _ := fs.Sub(embeddedFS, "static")
	r.StaticFS("/static", http.FS(sub))

	r.GET("/", root)
	r.POST("/info", getInfoGin)
	r.POST("/download", startDownloadGin)
	r.POST("/cancel/:id", cancelDownloadGin)
	r.GET("/progress/:id", progressGin)
	r.GET("/download/:id", serveFileGin)

	log.Println("http://localhost:9191")
	r.Run(":9191")
}
