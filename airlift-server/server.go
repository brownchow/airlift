package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"code.google.com/p/go.crypto/sha3"

	"github.com/moshee/gas"
	"github.com/moshee/gas/auth"
	"github.com/moshee/gas/out"
)

var (
	appDir        string
	fileList      *FileList
	defaultConfig = Config{
		Host: "",
		Port: 60606,
	}
	configLock sync.RWMutex
	flagPort   = flag.Int("p", 0, "Override port in config")
)

func init() {
	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	appDir = filepath.Join(u.HomeDir, ".airlift-server")
	defaultConfig.Directory = filepath.Join(appDir, "uploads")
	flag.Parse()
}

func main() {
	sessDir := filepath.Join(appDir, "sessions")
	os.RemoveAll(sessDir)
	store := &auth.FileStore{Root: sessDir}

	gas.AddDestructor(store.Destroy)

	auth.UseSessionStore(store)

	conf, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	fileList, err = conf.loadFiles()
	if err != nil {
		log.Fatalln("loading files:", err)
	}

	go fileList.watchAges(conf)

	r := gas.New()

	if gas.Env.TLSPort > 0 {
		r.Use(redirectTLS)
	}

	gas.Env.Port = conf.Port

	if *flagPort > 0 {
		gas.Env.Port = *flagPort
	}

	r.UseMore(out.CheckReroute).
		Get("/login", getLogin).
		Get("/logout", getLogout).
		Post("/login", checkConfig, postLogin).
		Get("/config", checkConfig, getConfig).
		Post("/config", checkConfig, postConfig).
		Post("/upload/file", checkConfig, checkPassword, postFile).
		Post("/oops", checkConfig, checkPassword, oops).
		Delete("/{id}", checkConfig, checkPassword, deleteFile).
		Get("/{id}/{filename}", checkConfig, getFile).
		Get("/{id}.{ext}", checkConfig, getFile).
		Get("/{id}", checkConfig, getFile).
		Ignition()
}

func checkConfig(g *gas.Gas) (int, gas.Outputter) {
	conf, err := loadConfig()
	if err != nil {
		log.Fatalln(g.Request.Method, "checkConfig:", err)
		return 500, out.JSON(&Resp{Err: err.Error()})
	}
	g.SetData("conf", conf)
	return g.Continue()
}

func checkPassword(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	if conf.Password != nil {
		pass := g.Request.Header.Get("X-Airlift-Password")
		if pass == "" {
			return 403, out.JSON(&Resp{Err: "password required"})
		}
		if !auth.VerifyHash([]byte(pass), conf.Password, conf.Salt) {
			return 403, out.JSON(&Resp{Err: "incorrect password"})
		}
	}

	return g.Continue()
}

func redirectTLS(g *gas.Gas) (int, gas.Outputter) {
	if g.TLS == nil && gas.Env.TLSPort > 0 {
		host := g.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		port := ""
		if gas.Env.TLSPort != 443 {
			port = ":" + strconv.Itoa(gas.Env.TLSPort)
		}
		return 302, out.Redirect(fmt.Sprintf("https://%s%s%s", host, port, g.URL.Path))
	}
	return g.Continue()
}

func (conf *Config) loadFiles() (*FileList, error) {
	files := new(FileList)
	// make sure the uploads folder is there, and then load all of the file
	// names and IDs into memory
	os.MkdirAll(conf.Directory, os.FileMode(0700))
	files.Files = make(map[string]os.FileInfo)
	list, err := ioutil.ReadDir(conf.Directory)
	if err != nil {
		return nil, err
	}
	for _, file := range list {
		parts := strings.SplitN(file.Name(), ".", 2)
		files.Files[parts[0]] = file
		files.Size += file.Size()
	}

	return files, nil
}

type FileList struct {
	Files map[string]os.FileInfo
	Size  int64
	sync.RWMutex
}

func (files *FileList) get(id string) string {
	files.RLock()
	defer files.RUnlock()
	file, ok := files.Files[id]
	if !ok {
		return ""
	}
	return file.Name()
}

// put creates a temp file, downloads a post body to it, moves it to the
// uploads, adds the file to the in-memory list, and returns the generated
// hash.
func (files *FileList) put(conf *Config, content io.Reader, filename string) (string, error) {
	dest := filepath.Join(conf.Directory, filename)
	destFile, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		os.Remove(dest)
		return "", err
	}

	defer destFile.Close()

	sha := sha3.NewKeccak256()
	w := io.MultiWriter(destFile, sha)
	io.Copy(w, content)
	hash := makeHash(sha.Sum(nil))

	destPath := filepath.Join(conf.Directory, hash+"."+filename)
	if err := os.Rename(dest, destPath); err != nil {
		os.Remove(dest)
		return "", err
	}

	fi, err := os.Stat(destPath)
	if err != nil {
		os.Remove(dest)
		return "", err
	}

	files.Lock()
	defer files.Unlock()
	files.Files[hash] = fi
	files.Size += fi.Size()

	if conf.MaxSize > 0 {
		files.pruneOldest(conf)
	}
	return hash, nil
}

func (files *FileList) pruneOldest(conf *Config) {
	ids := make([]string, 0, len(files.Files))
	for id := range files.Files {
		ids = append(ids, id)
	}

	sort.Sort(byModtime(ids))
	pruned := int64(0)
	n := 0
	for i := 0; files.Size > conf.MaxSize*1024*1024 && i < len(ids); i++ {
		id := ids[i]
		f := files.Files[id]
		if err := files.remove(conf, id); err != nil {
			log.Printf("pruning %s: %v", f.Name(), err)
			continue
		}
		files.Size -= f.Size()
		pruned += f.Size()
		n++
	}
	if n > 0 {
		log.Printf("Pruned %d uploads (%.2fMB) to keep under %dMB",
			n, float64(pruned)/(1024*1024), conf.MaxSize)
	}
}

func (files *FileList) pruneNewest(conf *Config) (string, error) {
	files.Lock()
	defer files.Unlock()

	if len(files.Files) == 0 {
		return "", nil
	}

	var newest os.FileInfo
	newestId := ""

	for id, fi := range files.Files {
		if newest == nil {
			newest = fi
		}
		if fi.ModTime().After(newest.ModTime()) {
			newest = fi
			newestId = id
		}
	}

	return newestId, files.remove(conf, newestId)
}

type byModtime []string

func (s byModtime) Len() int { return len(s) }
func (s byModtime) Less(i, j int) bool {
	a := fileList.Files[s[i]].ModTime()
	b := fileList.Files[s[j]].ModTime()
	return a.Before(b)
}
func (s byModtime) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (files *FileList) watchAges(conf *Config) {
	for {
		before := time.Now()
		if conf.MaxAge > 0 {
			cutoff := before.Add(-time.Duration(conf.MaxAge) * 24 * time.Hour)
			files.pruneOld(conf, cutoff)
		}
		after := time.Now()
		// execute next on the nearest day
		time.Sleep(before.AddDate(0, 0, 1).Truncate(24 * time.Hour).Sub(after))
	}
}

func (files *FileList) pruneOld(conf *Config, cutoff time.Time) {
	files.Lock()
	defer files.Unlock()
	n := 0
	for id, fi := range files.Files {
		if fi.ModTime().Before(cutoff) {
			if err := files.remove(conf, id); err != nil {
				log.Printf("Error pruning %s: %v", fi.Name(), err)
				continue
			}
		}
	}
	if n > 0 {
		log.Printf("%d upload(s) modified before %s pruned.", n, cutoff.Format("2006-01-02"))
	}
}

func (files *FileList) remove(conf *Config, id string) error {
	fi, ok := files.Files[id]
	if !ok {
		return fmt.Errorf("File id %s doesn't exist", id)
	}

	name := filepath.Join(conf.Directory, fi.Name())
	err := os.Remove(name)
	if err != nil {
		return err
	}

	delete(files.Files, id)
	return nil
}

type Config struct {
	Host      string
	Port      int
	Password  []byte
	Salt      []byte
	Directory string
	MaxAge    int   // max age of uploads in days
	MaxSize   int64 // max total size of uploads in MB
}

// satisfies gas.User interface
func (c Config) Secrets() (pass, salt []byte, err error) {
	return c.Password, c.Salt, nil
}

func (c Config) Username() string {
	return ""
}

// Update the config with the new password hash, generating a new random salt
func (c *Config) setPass(pass string) {
	c.Salt = make([]byte, 32)
	rand.Read(c.Salt)
	c.Password = auth.Hash([]byte(pass), c.Salt)
}

func loadConfig() (*Config, error) {
	if err := os.MkdirAll(appDir, os.FileMode(0700)); err != nil {
		return nil, err
	}
	var conf Config

	confPath := filepath.Join(appDir, "config")
	confFile, err := os.Open(confPath)
	if err != nil {
		if os.IsNotExist(err) {
			conf = defaultConfig
			err = writeConfig(&conf, confPath)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("reading config: %v", err)
		}
	} else {
		configLock.RLock()
		defer configLock.RUnlock()
		b, err := ioutil.ReadAll(confFile)
		if err != nil {
			return nil, fmt.Errorf("reading config: %v", err)
		}
		err = json.Unmarshal(b, &conf)
		if err != nil {
			return nil, fmt.Errorf("decoding config: %v", err)
		}
	}

	return &conf, nil
}

func writeConfig(conf *Config, to string) error {
	b, err := json.MarshalIndent(conf, "", "    ")
	if err != nil {
		return fmt.Errorf("encoding config: %v", err)
	}
	configLock.Lock()
	defer configLock.Unlock()
	file, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(0600))
	if err != nil {
		return fmt.Errorf("writing config: %v", err)
	}
	defer file.Close()
	file.Write(b)
	return nil
}

func getConfig(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	// if there's a password set, only allow user into config if they're logged
	// in, otherwise it's probably the first run and they need to enter one
	if conf.Password != nil {
		if sess, _ := auth.GetSession(g); sess == nil {
			return 303, out.Reroute("/login", "/config")
		}
	}

	return 200, out.HTML("config", conf, "common")
}

func postConfig(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	conf.Host = g.FormValue("host")
	conf.Directory = g.FormValue("directory")

	if conf.Password == nil {
		pass := g.FormValue("newpass")
		if pass == "" {
			return 400, out.JSON(&Resp{Err: "cannot set empty password"})
		} else {
			conf.setPass(pass)
		}
	} else {
		got := g.FormValue("password")
		if got == "" {
			return 403, out.JSON(&Resp{Err: "you forgot your password"})
		}
		if !auth.VerifyHash([]byte(got), conf.Password, conf.Salt) {
			return 403, out.JSON(&Resp{Err: "incorrect password"})
		}
	}

	port, err := strconv.Atoi(g.FormValue("port"))
	if err != nil {
		return 400, out.JSON(&Resp{Err: err.Error()})
	}
	conf.Port = port

	sage := g.FormValue("max-age")
	if len(sage) == 0 {
		conf.MaxAge = 0
	} else {
		age, err := strconv.Atoi(sage)
		if err != nil {
			return 400, out.JSON(&Resp{Err: err.Error()})
		}
		if age < 0 {
			age = 0
		}
		conf.MaxAge = age
	}

	ssize := g.FormValue("max-size")
	if len(ssize) == 0 {
		conf.MaxSize = 0
	} else {
		size, err := strconv.ParseInt(ssize, 10, 64)
		if err != nil {
			return 400, out.JSON(&Resp{Err: err.Error()})
		}
		if size < 0 {
			size = 0
		}
		conf.MaxSize = size
	}

	path := filepath.Join(appDir, "config")
	err = writeConfig(conf, path)
	if err != nil {
		log.Fatalln(g.Request.Method, "postConfig:", err)
		return 500, out.JSON(&Resp{Err: err.Error()})
	}

	return 204, nil
}

func getLogin(g *gas.Gas) (int, gas.Outputter) {
	// already logged in
	if sess, _ := auth.GetSession(g); sess != nil {
		return 302, out.Redirect("/config")
	}

	conf, err := loadConfig()
	if err == nil {
		if conf.Password == nil {
			return 302, out.Redirect("/config")
		}
	}

	return 200, out.HTML("login", false, "common")
}

func postLogin(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)
	var path string
	ok := out.Recover(g, &path) == nil

	if err := auth.SignIn(g, conf); err != nil {
		return 200, out.HTML("login", true, "common")
	}

	if !ok {
		path = "/config"
	}
	return 302, out.Redirect(path)
}

func getLogout(g *gas.Gas) (int, gas.Outputter) {
	if err := auth.SignOut(g); err != nil {
		log.Fatalln(g.Request.Method, "getLogout:", err)
		return 500, out.Error(g, err)
	}
	return 302, out.Redirect("/login")
}

func getFile(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)
	file := fileList.get(g.Arg("id"))
	if file == "" {
		return 404, out.Error(g, errors.New("ID not found"))
	}

	if g.Arg("filename") == "" {
		filename := url.QueryEscape(strings.SplitN(file, ".", 2)[1])
		g.Header().Set("Content-Disposition", "filename="+filename)
	}

	path := filepath.Join(conf.Directory, file)
	http.ServeFile(g, g.Request, path)

	return -1, nil
}

type Resp struct {
	URL string `json:",omitempty"`
	Err string `json:",omitempty"`
}

func postFile(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	filename := g.Request.Header.Get("X-Airlift-Filename")
	if filename == "" {
		return 400, out.JSON(&Resp{Err: "missing filename header"})
	}
	defer g.Body.Close()

	hash, err := fileList.put(conf, g.Body, filename)
	if err != nil {
		log.Fatalln(g.Request.Method, "postFile:", err)
		return 500, out.JSON(&Resp{Err: err.Error()})
	}

	host := conf.Host
	if host == "" {
		host = g.Request.Host
	}
	return 201, out.JSON(&Resp{URL: path.Join(host, hash)})
}

func makeHash(hash []byte) string {
	const (
		hashLen = 4
		chars   = "abcdefghijklmnopqrstuvwxyzZYXWVUTSRQPONMLKJIHGFEDCBA1234567890"
	)

	s := make([]byte, hashLen)

	for i, b := range hash {
		s[i%hashLen] ^= b
	}
	for i := range s {
		s[i] = chars[int(s[i])%len(chars)]
	}

	return string(s)
}

func deleteFile(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	id := g.Arg("id")
	if id == "" {
		return 400, out.JSON(&Resp{Err: "file ID not specified"})
	}

	fileList.Lock()
	defer fileList.Unlock()
	if err := fileList.remove(conf, id); err != nil {
		log.Fatalln(g.Request.Method, "deleteFile:", err)
		return 500, out.JSON(&Resp{Err: err.Error()})
	}

	return 204, nil
}

func oops(g *gas.Gas) (int, gas.Outputter) {
	conf := g.Data("conf").(*Config)

	pruned, err := fileList.pruneNewest(conf)
	if err != nil {
		log.Fatalln(g.Request.Method, "oops:", err)
		return 500, out.JSON(&Resp{Err: err.Error()})
	}

	host := conf.Host
	if host == "" {
		host = g.Request.Host
	}
	return 200, out.JSON(&Resp{URL: path.Join(host, pruned)})
}
