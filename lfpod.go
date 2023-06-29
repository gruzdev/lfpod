// Copyright 2023 Mikhail Gruzdev <michail.gruzdev@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/feeds"
	"github.com/gorilla/mux"
)

type YtMedia struct {
	XMLName     xml.Name `xml:"group"`
	Description string   `xml:"description"`
}

type YtEntry struct {
	XMLName   xml.Name `xml:"entry"`
	Title     string   `xml:"title"`
	VideoId   string   `xml:"videoId"`
	Published string   `xml:"published"`
	Media     *YtMedia `xml:"group"`
}

type YtFeed struct {
	XMLName xml.Name   `xml:"feed"`
	Entries []*YtEntry `xml:"entry"`
}

func readFeed(channelId string) ([]byte, error) {
	path := "https://www.youtube.com/feeds/videos.xml?channel_id=" + channelId
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{
		Timeout: 3000 * time.Millisecond,
	}
	res, err := client.Do(req)
	if err == nil {
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			err = errors.New("server response status " + res.Status)
		} else {
			body, err := ioutil.ReadAll(res.Body)
			if err == nil {
				return body, err
			}
		}
	}
	return nil, err
}

func parseFeed(data []byte, keywords []string) YtFeed {
	ytfeed := YtFeed{}
	if err := xml.Unmarshal(data, &ytfeed); err != nil {
		log.Fatal(err)
	}
	if keywords == nil {
		return ytfeed
	}
	f := YtFeed{}
	for _, entry := range ytfeed.Entries {
		for _, k := range keywords {
			tl, kl := strings.ToLower(entry.Title), strings.ToLower(k)
			if strings.Contains(tl, kl) {
				f.Entries = append(f.Entries, entry)
				break
			}
		}
	}
	return f
}

func downloadAudio(videoId string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	outFile := videoId
	cmd := exec.CommandContext(ctx, downloader, "-f", "worstaudio", "-x", "-o", "%(id)s", "--", videoId)
	cmd.Dir, _ = os.Getwd()
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outFile)
		log.Printf("%s", out)
	}
	return outFile, err
}

func getAudioFileName(channelId, videoId string) string {
	format := "opus"
	return filepath.Join("audio", channelId, videoId+"."+format)
}

func isVideoReady(videoId string) bool {
	cmd := exec.Command(downloader, "--no-warnings", "--print", "live_status", "--", videoId)
	cmd.Dir, _ = os.Getwd()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, "not_live") || strings.Contains(s, "was_live")
}

func recodeAudio(fileIn, fileOut string) {
	rate := "16k"
	fileTmp := "tmp.opus"
	cmd := exec.Command(converter, "-i", fileIn, "-c:a", "libopus", "-b:a", rate, "-y", fileTmp)
	cmd.Dir, _ = os.Getwd()
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("%s", out)
		log.Fatal(err)
	}
	if err := os.Rename(fileTmp, fileOut); err != nil {
		log.Fatal(err)
	}
}

func doUpdate(conf *Conf) {
	for _, feed := range conf.Feeds {
		data, err := readFeed(feed.ChannelId)
		if err != nil {
			log.Print(err)
			continue
		}
		ytfeed := parseFeed(data, feed.Keywords)
		for _, entry := range ytfeed.Entries {
			fileDst := getAudioFileName(feed.ChannelId, entry.VideoId)
			if _, err := os.Stat(fileDst); err == nil {
				continue
			}
			desc := feed.Name + " " + entry.VideoId
			log.Print("found new video ", desc)
			if !isVideoReady(entry.VideoId) {
				log.Print(desc, " not ready, skipped")
				continue
			}
			log.Print("downloading ", desc)
			if fileDown, err := downloadAudio(entry.VideoId); err != nil {
				log.Print(desc, " download error, skipped")
			} else {
				log.Print(desc, " downloaded")
				log.Print("recoding ", desc)
				recodeAudio(fileDown, fileDst)
				os.Remove(fileDown)
				log.Print(desc, " recoded")
			}
		}
	}
}

func updateFeeds(conf *Conf) {
	for {
		doUpdate(conf)
		time.Sleep(30 * time.Minute)
	}
}

func feedGetHandler(conf *Conf, w http.ResponseWriter, r *http.Request) {
	path, _ := url.JoinPath("http://", conf.ServerAddress, "feed")
	feedOut := &feeds.Feed{
		Title: "low-fi podcast",
		Link:  &feeds.Link{Href: path},
	}
	for _, feed := range conf.Feeds {
		data, err := readFeed(feed.ChannelId)
		if err != nil {
			log.Print(err)
			continue
		}
		ytfeed := parseFeed(data, feed.Keywords)
		for _, entry := range ytfeed.Entries {
			name := getAudioFileName(feed.ChannelId, entry.VideoId)
			if fileInfo, err := os.Stat(name); err == nil {
				fileSize := strconv.FormatInt(fileInfo.Size(), 10)
				path, _ = url.JoinPath("http://", conf.ServerAddress, "audio", feed.ChannelId, entry.VideoId+".opus")
				published, err := time.Parse(time.RFC3339, entry.Published)
				if err != nil {
					log.Fatal(err)
				}
				item := &feeds.Item{
					Title:       entry.Title,
					Link:        &feeds.Link{Href: path},
					Description: entry.Media.Description,
					Updated:     published,
					Created:     published,
					Enclosure:   &feeds.Enclosure{Url: path, Length: fileSize, Type: "audio/opus"},
				}
				feedOut.Add(item)
			}
		}
	}
	if err := feedOut.WriteAtom(w); err != nil {
		log.Fatal(err)
	}
}

func feedGetHadlerWrapper(conf *Conf) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		feedGetHandler(conf, w, r)
	}
}

var downloader = "yt-dlp"
var converter = "ffmpeg"
var probe = "ffprobe"

func checkExecs(execs ...*string) {
	for _, name := range execs {
		if _, err := exec.LookPath(*name); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				log.Fatalf("%s executable not found", *name)
			} else if errors.Is(err, exec.ErrDot) {
				log.Printf("%s executable found in current directory", *name)
				*name = "./" + *name
			} else {
				log.Fatal(err)
			}
		}
	}
}

type ConfFeed struct {
	Name      string   `json:"name"`
	ChannelId string   `json:"channel_id"`
	Keywords  []string `json:"keywords"`
}

type ConfFeeds struct {
	Feeds []ConfFeed `json:"ytfeeds"`
}

type Conf struct {
	ConfFeeds
	ServerAddress string
}

func readConfFeeds(fileName string) ConfFeeds {
	freader, err := os.Open(fileName)
	if err != nil {
		log.Fatal(err)
	}
	defer freader.Close()
	conf := ConfFeeds{}
	if err := json.NewDecoder(freader).Decode(&conf); err != nil {
		log.Fatal("error while parsing ", fileName, ": ", err)
	}
	return conf
}

func main() {
	confFeedsFile := flag.String("f", "ytfeeds.json", "YouTube feeds configuration file.")
	serverAddress := flag.String("s", "127.0.0.1:8080", "Server address.")
	flag.Parse()

	conf := Conf{readConfFeeds(*confFeedsFile), *serverAddress}

	checkExecs(&downloader, &converter, &probe)

	for _, feed := range conf.Feeds {
		if err := os.MkdirAll(filepath.Join("audio", feed.ChannelId), 0750); err != nil {
			log.Fatal(err)
		}
	}

	go updateFeeds(&conf)

	r := mux.NewRouter()
	r.HandleFunc("/feed", feedGetHadlerWrapper(&conf)).Methods("GET")
	r.PathPrefix("/audio/").Handler(http.StripPrefix("/audio/", http.FileServer(http.Dir("audio"))))
	log.Fatal(http.ListenAndServe(":8080", r))
}
