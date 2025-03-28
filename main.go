package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"main/utils/lyrics"
	"main/utils/runv2"
	"main/utils/runv3"
	"main/utils/structs"

	"github.com/fatih/color"
	"github.com/grafov/m3u8"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/pflag"
	"github.com/zhaarey/go-mp4tag"
	"gopkg.in/yaml.v2"
)

const maxNameLength = 100

// getStorefrontFromURL extracts the storefront from an Apple Music URL.
func getStorefrontFromURL(urlStr string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) >= 2 {
		return parts[1], nil // parts[0] is "", parts[1] is storefront (e.g., "us")
	}
	return "", errors.New("invalid URL format")
}

var (
	forbiddenNames = regexp.MustCompile(`[/\\<>:"|?*]`)
	dl_atmos       bool
	dl_aac         bool
	dl_select      bool
	dl_song        bool
	artist_select  bool
	debug_mode     bool
	lyrics_only    bool
	atmos_only     bool
	skip_mv        bool
	cover_art_only bool // New flag for downloading only cover art
	alac_max       *int
	atmos_max      *int
	mv_max         *int
	mv_audio_type  *string
	aac_type       *string
	Config         structs.ConfigSet
	counter        structs.Counter
	okDict         = make(map[string][]int)
)

func loadConfig() error {
	// 读取config.yaml文件内容
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		return err
	}
	// 将yaml解析到config变量中
	err = yaml.Unmarshal(data, &Config)
	if err != nil {
		return err
	}
	return nil
}

func LimitString(s string) string {
	if len([]rune(s)) > Config.LimitMax {
		return string([]rune(s)[:Config.LimitMax])
	}
	return s
}

func isInArray(arr []int, target int) bool {
	for _, num := range arr {
		if num == target {
			return true
		}
	}
	return false
}

func fileExists(path string) (bool, error) {
	f, err := os.Stat(path)
	if err == nil {
		return !f.IsDir(), nil
	} else if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func checkUrl(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/album|\/album\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlMv(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/music-video|\/music-video\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlSong(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/song|\/song\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlPlaylist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/playlist|\/playlist\/.+))\/(?:id)?(pl\.[\w-]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func checkUrlArtist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/artist|\/artist\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func getUrlSong(songUrl string, token string) (string, error) {
	storefront, songId := checkUrlSong(songUrl)
	manifest, err := getInfoFromAdam(songId, token, storefront)
	if err != nil {
		fmt.Println("\u26A0 Failed to get manifest:", err)
		counter.NotSong++
		return "", err
	}
	albumId := manifest.Relationships.Albums.Data[0].ID
	songAlbumUrl := fmt.Sprintf("https://music.apple.com/%s/album/1/%s?i=%s", storefront, albumId, songId)
	return songAlbumUrl, nil
}
func getUrlArtistName(artistUrl string, token string) (string, string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s", storefront, artistId), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Origin", "https://music.apple.com")
	query := url.Values{}
	query.Set("l", Config.Language)
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return "", "", errors.New(do.Status)
	}
	obj := new(structs.AutoGeneratedArtist)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return "", "", err
	}
	return obj.Data[0].Attributes.Name, obj.Data[0].ID, nil
}

func checkArtist(artistUrl string, token string, relationship string) ([]string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	Num := 0
	//id := 1
	var args []string
	var urls []string
	var options [][]string
	for {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s/%s?limit=100&offset=%d&l=%s", storefront, artistId, relationship, Num, Config.Language), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		req.Header.Set("Origin", "https://music.apple.com")
		do, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer do.Body.Close()
		if do.StatusCode != http.StatusOK {
			return nil, errors.New(do.Status)
		}
		obj := new(structs.AutoGeneratedArtist)
		err = json.NewDecoder(do.Body).Decode(&obj)
		if err != nil {
			return nil, err
		}
		for _, album := range obj.Data {
			options = append(options, []string{album.Attributes.Name, album.Attributes.ReleaseDate, album.ID, album.Attributes.URL})
		}
		Num = Num + 100
		if len(obj.Next) == 0 {
			break
		}
	}
	sort.Slice(options, func(i, j int) bool {
		// 将日期字符串解析为 time.Time 类型进行比较
		dateI, _ := time.Parse("2006-01-02", options[i][1])
		dateJ, _ := time.Parse("2006-01-02", options[j][1])
		return dateI.Before(dateJ) // 返回 true 表示 i 在 j 前面
	})

	table := tablewriter.NewWriter(os.Stdout)
	if relationship == "albums" {
		table.SetHeader([]string{"", "Album Name", "Date", "Album ID"})
	} else if relationship == "music-videos" {
		table.SetHeader([]string{"", "MV Name", "Date", "MV ID"})
	}
	//table.SetFooter([]string{"", "", "Total", "$146.93"})
	//table.SetAutoMergeCells(true)
	//table.SetAutoMergeCellsByColumnIndex([]int{1,2,3})
	table.SetRowLine(false)
	//table.AppendBulk(options)
	table.SetHeaderColor(tablewriter.Colors{},
		tablewriter.Colors{tablewriter.FgRedColor, tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})

	table.SetColumnColor(tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgRedColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})
	for i, v := range options {
		urls = append(urls, v[3])
		options[i] = append([]string{fmt.Sprint(i + 1)}, v[:3]...)
		table.Append(options[i])
	}
	table.Render()
	if artist_select {
		fmt.Println("You have selected all options:")
		return urls, nil
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Please select from the " + relationship + " options above (multiple options separated by commas, ranges supported, or type 'all' to select all)")
	cyanColor := color.New(color.FgCyan)
	cyanColor.Print("Enter your choice: ")
	input, _ := reader.ReadString('\n')

	// Remove newline and whitespace
	input = strings.TrimSpace(input)
	if input == "all" {
		fmt.Println("You have selected all options:")
		return urls, nil
	}

	// Split input into string slices
	selectedOptions := [][]string{}
	parts := strings.Split(input, ",")
	for _, part := range parts {
		if strings.Contains(part, "-") { // Range setting
			rangeParts := strings.Split(part, "-")
			selectedOptions = append(selectedOptions, rangeParts)
		} else { // Single option
			selectedOptions = append(selectedOptions, []string{part})
		}
	}

	// Print selected options
	fmt.Println("You have selected the following options:")
	for _, opt := range selectedOptions {
		if len(opt) == 1 { // Single option
			num, err := strconv.Atoi(opt[0])
			if err != nil {
				fmt.Println("Invalid option:", opt[0])
				continue
			}
			if num > 0 && num <= len(options) {
				fmt.Println(options[num-1])
				args = append(args, urls[num-1])
			} else {
				fmt.Println("Option out of range:", opt[0])
			}
		} else if len(opt) == 2 { // Range
			start, err1 := strconv.Atoi(opt[0])
			end, err2 := strconv.Atoi(opt[1])
			if err1 != nil || err2 != nil {
				fmt.Println("Invalid range:", opt)
				continue
			}
			if start < 1 || end > len(options) || start > end {
				fmt.Println("Range out of range:", opt)
				continue
			}
			for i := start; i <= end; i++ {
				fmt.Println(options[i-1])
				args = append(args, urls[i-1])
			}
		} else {
			fmt.Println("Invalid option:", opt)
		}
	}
	return args, nil
}

func getMeta(albumId string, token string, storefront string) (*structs.AutoGenerated, error) {
	var mtype string
	var next string
	if strings.Contains(albumId, "pl.") {
		mtype = "playlists"
	} else {
		mtype = "albums"
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/%s/%s", storefront, mtype, albumId), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Origin", "https://music.apple.com")
	query := url.Values{}
	query.Set("omit[resource]", "autos")
	query.Set("include", "tracks,artists,record-labels")
	query.Set("include[songs]", "artists,albums")
	query.Set("fields[artists]", "name,artwork")
	query.Set("fields[albums:albums]", "artistName,artwork,name,releaseDate,url")
	query.Set("fields[record-labels]", "name")
	query.Set("extend", "editorialVideo")
	query.Set("l", Config.Language)
	req.URL.RawQuery = query.Encode()
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return nil, errors.New(do.Status)
	}
	obj := new(structs.AutoGenerated)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return nil, err
	}
	if strings.Contains(albumId, "pl.") {
		obj.Data[0].Attributes.ArtistName = "Apple Music"
		if len(obj.Data[0].Relationships.Tracks.Next) > 0 {
			next = obj.Data[0].Relationships.Tracks.Next
			for {
				req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/%s&l=%s&include=albums", next, Config.Language), nil)
				if err != nil {
					return nil, err
				}
				req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
				req.Header.Set("Origin", "https://music.apple.com")
				do, err := http.DefaultClient.Do(req)
				if err != nil {
					return nil, err
				}
				defer do.Body.Close()
				if do.StatusCode != http.StatusOK {
					return nil, errors.New(do.Status)
				}
				obj2 := new(structs.AutoGeneratedTrack)
				err = json.NewDecoder(do.Body).Decode(&obj2)
				if err != nil {
					return nil, err
				}
				for _, value := range obj2.Data {
					obj.Data[0].Relationships.Tracks.Data = append(obj.Data[0].Relationships.Tracks.Data, value)
				}
				next = obj2.Next
				if len(next) == 0 {
					break
				}
			}
		}
	}
	return obj, nil
}

func writeCover(sanAlbumFolder, name string, url string) (string, error) {
	// Validate inputs
	if sanAlbumFolder == "" {
		return "", errors.New("Error: Empty album folder path provided")
	}
	if name == "" {
		return "", errors.New("Error: Empty cover name provided")
	}
	if url == "" {
		return "", errors.New("Error: Empty cover URL provided")
	}

	// Format URL and determine output path
	covPath := filepath.Join(sanAlbumFolder, name+"."+Config.CoverFormat)
	originalUrl := url

	if Config.CoverFormat == "original" {
		// Extract extension from URL
		urlParts := strings.Split(url, "/")
		if len(urlParts) < 3 {
			return "", fmt.Errorf("Invalid URL format: %s", url)
		}

		extPart := urlParts[len(urlParts)-2]
		extIndex := strings.LastIndex(extPart, ".")
		if extIndex < 0 {
			return "", fmt.Errorf("Could not determine extension from URL: %s", url)
		}

		ext := extPart[extIndex+1:]
		if ext == "" {
			ext = "jpg" // Default to jpg if extraction fails
			fmt.Printf("Warning: Could not extract extension from URL %s, defaulting to jpg\n", url)
		}
		covPath = filepath.Join(sanAlbumFolder, name+"."+ext)
	}

	// Check if cover already exists
	exists, err := fileExists(covPath)
	if err != nil {
		return "", fmt.Errorf("Failed to check if cover exists at %s: %w", covPath, err)
	}

	// Remove existing file if it exists
	if exists {
		err = os.Remove(covPath)
		if err != nil {
			return "", fmt.Errorf("Failed to remove existing cover at %s: %w", covPath, err)
		}
	}

	// Process URL based on format requirements
	if Config.CoverFormat == "png" {
		re := regexp.MustCompile(`\{w\}x\{h\}`)
		if !re.MatchString(url) {
			return "", fmt.Errorf("URL does not contain {w}x{h} placeholder: %s", url)
		}

		parts := re.Split(url, 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("Failed to split URL on {w}x{h}: %s", url)
		}

		url = parts[0] + "{w}x{h}" + strings.Replace(parts[1], ".jpg", ".png", 1)
	}

	// Replace dimensions in URL
	url = strings.Replace(url, "{w}x{h}", Config.CoverSize, 1)

	// Special handling for original format
	if Config.CoverFormat == "original" {
		url = strings.Replace(url, "is1-ssl.mzstatic.com/image/thumb", "a5.mzstatic.com/us/r1000/0", 1)
		lastSlashIndex := strings.LastIndex(url, "/")
		if lastSlashIndex < 0 {
			return "", fmt.Errorf("Invalid URL format for original cover: %s", url)
		}
		url = url[:lastSlashIndex]
	}

	fmt.Printf("Processing cover: %s -> %s\n", name, covPath)
	fmt.Printf("Cover URL: %s\n", url)

	// Create HTTP request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("Failed to create HTTP request for URL %s: %w", url, err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

	// Execute request
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		// Try with original URL as fallback
		if url != originalUrl {
			fmt.Printf("Failed with processed URL, trying original URL: %s\n", originalUrl)
			req, err = http.NewRequest("GET", originalUrl, nil)
			if err != nil {
				return "", fmt.Errorf("Failed to create HTTP request for fallback URL: %w", err)
			}

			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
			do, err = http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("Network error downloading cover from both URLs: %w", err)
			}
		} else {
			return "", fmt.Errorf("Network error downloading cover: %w", err)
		}
	}

	defer do.Body.Close()

	// Check HTTP status
	if do.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Server returned non-OK status downloading cover: %s (HTTP %d)", do.Status, do.StatusCode)
	}

	// Check content length
	contentLength := do.ContentLength
	if contentLength <= 0 {
		fmt.Println("Warning: Content length not available or zero, proceeding anyway")
	} else if contentLength < 1000 {
		fmt.Printf("Warning: Cover image is very small (%d bytes), might be a placeholder\n", contentLength)
	}

	// Create destination file
	f, err := os.Create(covPath)
	if err != nil {
		return "", fmt.Errorf("Failed to create cover file at %s: %w", covPath, err)
	}
	defer f.Close()

	// Copy content
	bytesWritten, err := io.Copy(f, do.Body)
	if err != nil {
		return "", fmt.Errorf("Failed to write cover data to file: %w", err)
	}

	if bytesWritten == 0 {
		return "", errors.New("Cover downloaded but file is empty (0 bytes)")
	}

	fmt.Printf("Cover downloaded successfully: %s (%d bytes)\n", covPath, bytesWritten)
	return covPath, nil
}

func writeLyrics(sanAlbumFolder, filename string, lrc string) error {
	lyricspath := filepath.Join(sanAlbumFolder, filename)
	f, err := os.Create(lyricspath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lrc)
	if err != nil {
		return err
	}
	return nil
}

func contains(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

// checkAlbumHasAtmos checks if an album has any Dolby Atmos tracks
func checkAlbumHasAtmos(meta *structs.AutoGenerated, token string, storefront string) (bool, error) {
	// Skip check for playlists
	if len(meta.Data) == 0 || meta.Data[0].Type == "playlists" {
		return false, nil
	}

	// Check at least 3 tracks or all if album has fewer tracks
	checkLimit := 3
	if len(meta.Data[0].Relationships.Tracks.Data) < checkLimit {
		checkLimit = len(meta.Data[0].Relationships.Tracks.Data)
	}

	for i := 0; i < checkLimit; i++ {
		track := meta.Data[0].Relationships.Tracks.Data[i]

		manifest, err := getInfoFromAdam(track.ID, token, storefront)
		if err != nil {
			continue // Skip this track if can't get manifest
		}

		if manifest.Attributes.ExtendedAssetUrls.EnhancedHls == "" {
			continue // Skip if no enhanced HLS URL
		}

		// Check for Atmos in the M3U8
		masterUrl := manifest.Attributes.ExtendedAssetUrls.EnhancedHls

		// Try device port for better detection if configured
		if Config.GetM3u8Mode == "all" || (Config.GetM3u8Mode == "hires" && contains(track.Attributes.AudioTraits, "hi-res-lossless")) {
			deviceM3u8, err := checkM3u8(track.ID, "song")
			if err == nil && strings.HasSuffix(deviceM3u8, ".m3u8") {
				masterUrl = deviceM3u8
			}
		}

		// Check if the track has Atmos
		hasAtmos, err := checkTrackHasAtmos(masterUrl)
		if err != nil {
			continue
		}

		if hasAtmos {
			return true, nil
		}
	}

	return false, nil
}

// checkTrackHasAtmos checks if a track's M3U8 contains Atmos variants
func checkTrackHasAtmos(masterUrl string) (bool, error) {
	resp, err := http.Get(masterUrl)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return false, errors.New("m3u8 not of master type")
	}

	master := from.(*m3u8.MasterPlaylist)

	// Check for Atmos variants
	for _, variant := range master.Variants {
		if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") {
			return true, nil
		}
	}

	return false, nil
}

// 下载单曲逻辑
func downloadTrack(trackNum int, trackTotal int, meta *structs.AutoGenerated, track structs.TrackData, albumId, token, storefront, mediaUserToken, sanAlbumFolder, Codec string, covPath string) {
	counter.Total++
	fmt.Printf("Track %d of %d:\n", trackNum, trackTotal)

	// Skip if cover_art_only mode is enabled
	if cover_art_only {
		// We already downloaded the album cover in rip(), no need to do anything per track
		counter.Success++
		return
	}

	//mv dl dev
	if track.Type == "music-videos" {
		if lyrics_only {
			fmt.Println("Skipping MV download (lyrics-only mode)")
			counter.Success++
			return
		}

		if len(mediaUserToken) <= 50 {
			fmt.Println("meida-user-token is not set, skip MV dl")
			counter.Success++
			return
		}
		if _, err := exec.LookPath("mp4decrypt"); err != nil {
			fmt.Println("mp4decrypt is not found, skip MV dl")
			counter.Success++
			return
		}

		// Skip if skip_mv flag is enabled
		if skip_mv {
			fmt.Println("Skipping MV download (skip-mv enabled)")
			counter.Success++
			return
		}

		err := mvDownloader(track.ID, sanAlbumFolder, token, storefront, mediaUserToken, meta)
		if err != nil {
			fmt.Println("\u26A0 Failed to dl MV:", err)
			counter.Error++
			return
		}
		counter.Success++
		return
	}

	manifest, err := getInfoFromAdam(track.ID, token, storefront)
	if err != nil {
		fmt.Println("\u26A0 Failed to get manifest:", err)
		counter.NotSong++
		return
	}
	needDlAacLc := false
	if dl_aac && Config.AacType == "aac-lc" {
		needDlAacLc = true
	}
	if manifest.Attributes.ExtendedAssetUrls.EnhancedHls == "" {
		if dl_atmos {
			fmt.Println("Unavailable")
			counter.Unavailable++
			return
		}
		fmt.Println("Unavailable, Try DL AAC-LC")
		needDlAacLc = true
	}
	needCheck := false

	if Config.GetM3u8Mode == "all" {
		needCheck = true
	} else if Config.GetM3u8Mode == "hires" && contains(track.Attributes.AudioTraits, "hi-res-lossless") {
		needCheck = true
	}
	var EnhancedHls_m3u8 string
	if needCheck && !needDlAacLc {
		EnhancedHls_m3u8, _ = checkM3u8(track.ID, "song")
		if strings.HasSuffix(EnhancedHls_m3u8, ".m3u8") {
			manifest.Attributes.ExtendedAssetUrls.EnhancedHls = EnhancedHls_m3u8
		}
	}
	var Quality string
	if strings.Contains(Config.SongFileFormat, "Quality") {
		if dl_atmos {
			Quality = fmt.Sprintf("%dkbps", Config.AtmosMax-2000)
		} else if needDlAacLc {
			Quality = "256kbps"
		} else {
			_, Quality, err = extractMedia(manifest.Attributes.ExtendedAssetUrls.EnhancedHls, true)
			if err != nil {
				fmt.Println("Failed to extract quality from manifest.\n", err)
				counter.Error++
				return
			}
		}
	}
	stringsToJoin := []string{}
	if track.Attributes.IsAppleDigitalMaster {
		if Config.AppleMasterChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.AppleMasterChoice)
		}
	}
	if track.Attributes.ContentRating == "explicit" {
		if Config.ExplicitChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.ExplicitChoice)
		}
	}
	if track.Attributes.ContentRating == "clean" {
		if Config.CleanChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.CleanChoice)
		}
	}
	Tag_string := strings.Join(stringsToJoin, " ")

	// In downloadTrack function
	songName := strings.NewReplacer(
		"{SongId}", track.ID,
		"{SongNumer}", fmt.Sprintf("%02d", trackNum),
		"{SongName}", LimitString(track.Attributes.Name),
		"{DiscNumber}", fmt.Sprintf("%0d", track.Attributes.DiscNumber),
		"{TrackNumber}", fmt.Sprintf("%0d", track.Attributes.TrackNumber),
		"{Quality}", Quality,
		"{Tag}", Tag_string,
		"{Codec}", Codec,
		"{ArtistName}", LimitString(track.Attributes.ArtistName),
	).Replace(Config.SongFileFormat)

	filename := fmt.Sprintf("%s.m4a", forbiddenNames.ReplaceAllString(songName, "_"))
	lrcFilename := fmt.Sprintf("%s.%s", forbiddenNames.ReplaceAllString(songName, "_"), Config.LrcFormat)

	// Check if filenames exceed maxNameLength and apply fallback using the config template
	if len(filename) > maxNameLength {
		songNameFallback := strings.NewReplacer(
			"{SongId}", track.ID,
			"{SongNumer}", fmt.Sprintf("%02d", trackNum),
			"{SongName}", track.ID,
			"{DiscNumber}", "",
			"{TrackNumber}", "",
			"{Quality}", "",
			"{Tag}", "",
			"{Codec}", "",
			"{ArtistName}", "",
		).Replace(Config.SongFileFormat)
		filename = fmt.Sprintf("%s.m4a", forbiddenNames.ReplaceAllString(songNameFallback, "_"))
	}

	if len(lrcFilename) > maxNameLength {
		songNameFallback := strings.NewReplacer(
			"{SongId}", track.ID,
			"{SongNumer}", fmt.Sprintf("%02d", trackNum),
			"{SongName}", track.ID,
			"{DiscNumber}", "",
			"{TrackNumber}", "",
			"{Quality}", "",
			"{Tag}", "",
			"{Codec}", "",
			"{ArtistName}", "",
		).Replace(Config.SongFileFormat)
		lrcFilename = fmt.Sprintf("%s.%s", forbiddenNames.ReplaceAllString(songNameFallback, "_"), Config.LrcFormat)
	}

	// Define trackPath here
	trackPath := filepath.Join(sanAlbumFolder, filename)

	// Get lyrics - now unconditionally downloaded in lyrics-only mode
	var lrc string = ""
	lyricsDownloaded := false
	if Config.EmbedLrc || Config.SaveLrcFile || lyrics_only {
		lrcStr, err := lyrics.Get(storefront, track.ID, Config.LrcType, Config.Language, Config.LrcFormat, token, mediaUserToken)
		if err != nil {
			fmt.Println(err)
		} else {
			lyricsDownloaded = true
			if Config.SaveLrcFile || lyrics_only {
				err := writeLyrics(sanAlbumFolder, lrcFilename, lrcStr)
				if err != nil {
					fmt.Printf("Failed to write lyrics")
				} else if lyrics_only {
					fmt.Printf("Lyrics saved to: %s\n", lrcFilename)
				}
			}
			if Config.EmbedLrc {
				lrc = lrcStr
			}
		}
	}

	// In lyrics-only mode, mark as success after downloading lyrics and skip the rest
	if lyrics_only {
		counter.Success++
		if !lyricsDownloaded {
			fmt.Println("No lyrics found for this track")
		}
		okDict[albumId] = append(okDict[albumId], trackNum)
		return
	}

	exists, err := fileExists(trackPath)
	if err != nil {
		fmt.Println("Failed to check if track exists.")
	}
	if exists {
		fmt.Println("Track already exists locally.")
		counter.Success++
		okDict[albumId] = append(okDict[albumId], trackNum)
		return
	}
	if needDlAacLc {
		if len(mediaUserToken) <= 50 {
			fmt.Println("Invalid media-user-token")
			counter.Error++
			return
		}
		_, err := runv3.Run(track.ID, trackPath, token, mediaUserToken, false)
		if err != nil {
			fmt.Println("Failed to dl aac-lc:", err)
			counter.Error++
			return
		}
	} else {
		trackM3u8Url, _, err := extractMedia(manifest.Attributes.ExtendedAssetUrls.EnhancedHls, false)
		if err != nil {
			fmt.Println("\u26A0 Failed to extract info from manifest:", err)
			counter.Unavailable++
			return
		}
		//边下载边解密
		err = runv2.Run(track.ID, trackM3u8Url, trackPath, Config)
		if err != nil {
			fmt.Println("Failed to run v2:", err)
			counter.Error++
			return
		}
	}
	tags := []string{
		"tool=",
		fmt.Sprintf("artist=%s", meta.Data[0].Attributes.ArtistName),
		//fmt.Sprintf("lyrics=%s", lrc),
	}
	var trackCovPath string
	if Config.EmbedCover {
		if strings.Contains(albumId, "pl.") && Config.DlAlbumcoverForPlaylist {
			trackCovPath, err = writeCover(sanAlbumFolder, track.ID, track.Attributes.Artwork.URL)
			if err != nil {
				fmt.Println("Failed to write cover.")
			}
			tags = append(tags, fmt.Sprintf("cover=%s", trackCovPath))
		} else {
			tags = append(tags, fmt.Sprintf("cover=%s", covPath))
		}
	}
	tagsString := strings.Join(tags, ":")
	cmd := exec.Command("MP4Box", "-itags", tagsString, trackPath)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Embed failed: %v\n", err)
		counter.Error++
		return
	}
	if strings.Contains(albumId, "pl.") && Config.DlAlbumcoverForPlaylist && trackCovPath != "" {
		if err := os.Remove(trackCovPath); err != nil {
			fmt.Printf("Error deleting file: %s\n", trackCovPath)
			counter.Error++
			return
		}
	}
	err = writeMP4Tags(trackPath, lrc, meta, trackNum, trackTotal)
	if err != nil {
		fmt.Println("\u26A0 Failed to write tags in media:", err)
		counter.Unavailable++
		return
	}
	counter.Success++
	okDict[albumId] = append(okDict[albumId], trackNum)
}

func rip(albumId string, token string, storefront string, mediaUserToken string, urlArg_i string) error {
	meta, err := getMeta(albumId, token, storefront)
	if err != nil {
		return err
	}

	// If cover_art_only flag is set, only download cover art
	if cover_art_only {
		fmt.Println("Cover art only mode enabled: Will only download album artwork")

		// Use the same folder structure logic as regular downloads
		var singerFoldername string
		if Config.ArtistFolderFormat != "" {
			if strings.Contains(albumId, "pl.") {
				singerFoldername = strings.NewReplacer(
					"{ArtistName}", "Apple Music",
					"{ArtistId}", "",
					"{UrlArtistName}", "Apple Music",
				).Replace(Config.ArtistFolderFormat)
			} else if len(meta.Data[0].Relationships.Artists.Data) > 0 {
				singerFoldername = strings.NewReplacer(
					"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
					"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
					"{ArtistId}", meta.Data[0].Relationships.Artists.Data[0].ID,
				).Replace(Config.ArtistFolderFormat)
			} else {
				singerFoldername = strings.NewReplacer(
					"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
					"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
					"{ArtistId}", "",
				).Replace(Config.ArtistFolderFormat)
			}
			if strings.HasSuffix(singerFoldername, ".") {
				singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
			}
			singerFoldername = strings.TrimSpace(singerFoldername)
			if len(singerFoldername) > maxNameLength {
				if len(meta.Data[0].Relationships.Artists.Data) > 0 && !strings.Contains(albumId, "pl.") {
					singerFoldername = meta.Data[0].Relationships.Artists.Data[0].ID
				} else {
					singerFoldername = "UnknownArtist"
				}
			}
			fmt.Println(singerFoldername)
		}

		singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))

		var albumFolder string
		if strings.Contains(albumId, "pl.") {
			albumFolder = strings.NewReplacer(
				"{ArtistName}", "Apple Music",
				"{PlaylistName}", LimitString(meta.Data[0].Attributes.Name),
				"{PlaylistId}", albumId,
				"{Quality}", "",
				"{Codec}", "ALAC",
				"{Tag}", "",
			).Replace(Config.PlaylistFolderFormat)
		} else {
			albumFolder = strings.NewReplacer(
				"{ReleaseDate}", meta.Data[0].Attributes.ReleaseDate,
				"{ReleaseYear}", meta.Data[0].Attributes.ReleaseDate[:4],
				"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{AlbumName}", LimitString(meta.Data[0].Attributes.Name),
				"{UPC}", meta.Data[0].Attributes.Upc,
				"{RecordLabel}", meta.Data[0].Attributes.RecordLabel,
				"{Copyright}", meta.Data[0].Attributes.Copyright,
				"{AlbumId}", albumId,
				"{Quality}", "",
				"{Codec}", "ALAC",
				"{Tag}", "",
			).Replace(Config.AlbumFolderFormat)
		}

		albumFolder = strings.TrimSpace(albumFolder)
		sanAlbumFolder := filepath.Join(singerFolder, forbiddenNames.ReplaceAllString(albumFolder, "_"))

		// Create album folder
		err = os.MkdirAll(sanAlbumFolder, 0755)
		if err != nil {
			return errors.New("Failed to create album folder.\n" + err.Error())
		}

		// Download cover
		fmt.Println("Downloading album artwork...")
		coverURL := meta.Data[0].Attributes.Artwork.URL
		_, err = writeCover(sanAlbumFolder, "cover", coverURL)
		if err != nil {
			return errors.New("Failed to download cover.\n" + err.Error())
		}

		fmt.Println("Cover artwork successfully downloaded!")
		counter.Success++
		return nil
	}

	// If atmos-only flag is set, check if album has Atmos tracks
	if atmos_only {
		// Set dl_atmos to true when atmos_only is enabled
		dl_atmos = true

		// Check if album has Atmos tracks
		hasAtmos, err := checkAlbumHasAtmos(meta, token, storefront)
		if err != nil {
			fmt.Println("Failed to check Atmos availability:", err)
		}

		if !hasAtmos {
			fmt.Println("Skipping album (no Dolby Atmos tracks found)")
			counter.Unavailable++
			return nil
		}

		fmt.Println("Album has Dolby Atmos tracks, continuing download...")
	}

	if debug_mode {
		// Print album info
		fmt.Println(meta.Data[0].Attributes.ArtistName)
		fmt.Println(meta.Data[0].Attributes.Name)

		for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
			trackNum++
			fmt.Printf("\nTrack %d of %d:\n", trackNum, len(meta.Data[0].Relationships.Tracks.Data))
			fmt.Printf("%02d. %s\n", trackNum, track.Attributes.Name)

			manifest, err := getInfoFromAdam(track.ID, token, storefront)
			if err != nil {
				fmt.Printf("Failed to get manifest for track %d: %v\n", trackNum, err)
				continue
			}

			var m3u8Url string
			//Web端m3u8
			if manifest.Attributes.ExtendedAssetUrls.EnhancedHls != "" {
				m3u8Url = manifest.Attributes.ExtendedAssetUrls.EnhancedHls
			}
			//设备端满血m3u8
			needCheck := false
			if Config.GetM3u8Mode == "all" {
				needCheck = true
			} else if Config.GetM3u8Mode == "hires" && contains(track.Attributes.AudioTraits, "hi-res-lossless") {
				needCheck = true
			}
			if needCheck {
				fullM3u8Url, err := checkM3u8(track.ID, "song")
				if err == nil && strings.HasSuffix(fullM3u8Url, ".m3u8") {
					m3u8Url = fullM3u8Url
				} else {
					fmt.Println("Failed to get best quality m3u8 from device m3u8 port, will use m3u8 from Web API")
				}
			}

			_, _, err = extractMedia(m3u8Url, true)
			if err != nil {
				fmt.Printf("Failed to extract quality info for track %d: %v\n", trackNum, err)
				continue
			}
		}
		return nil // Return directly without showing statistics
	}
	var Codec string
	if dl_atmos {
		Codec = "ATMOS"
	} else if dl_aac {
		Codec = "AAC"
	} else {
		Codec = "ALAC"
	}
	var singerFoldername string
	if Config.ArtistFolderFormat != "" {
		if strings.Contains(albumId, "pl.") {
			singerFoldername = strings.NewReplacer(
				"{ArtistName}", "Apple Music",
				"{ArtistId}", "",
				"{UrlArtistName}", "Apple Music",
			).Replace(Config.ArtistFolderFormat)
		} else if len(meta.Data[0].Relationships.Artists.Data) > 0 {
			singerFoldername = strings.NewReplacer(
				"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistId}", meta.Data[0].Relationships.Artists.Data[0].ID,
			).Replace(Config.ArtistFolderFormat)
		} else {
			singerFoldername = strings.NewReplacer(
				"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistId}", "",
			).Replace(Config.ArtistFolderFormat)
		}
		if strings.HasSuffix(singerFoldername, ".") {
			singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
		}
		singerFoldername = strings.TrimSpace(singerFoldername)
		if len(singerFoldername) > maxNameLength {
			if len(meta.Data[0].Relationships.Artists.Data) > 0 && !strings.Contains(albumId, "pl.") {
				singerFoldername = meta.Data[0].Relationships.Artists.Data[0].ID
			} else {
				singerFoldername = "UnknownArtist"
			}
		}
		fmt.Println(singerFoldername)
	}
	singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	if dl_atmos {
		singerFolder = filepath.Join(Config.AtmosSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	var Quality string
	if strings.Contains(Config.AlbumFolderFormat, "Quality") {
		if dl_atmos {
			Quality = fmt.Sprintf("%dkbps", Config.AtmosMax-2000)
		} else if dl_aac && Config.AacType == "aac-lc" {
			Quality = "256kbps"
		} else {
			manifest1, err := getInfoFromAdam(meta.Data[0].Relationships.Tracks.Data[0].ID, token, storefront)
			if err != nil {
				fmt.Println("Failed to get manifest.\n", err)
			} else {
				if manifest1.Attributes.ExtendedAssetUrls.EnhancedHls == "" {
					Codec = "AAC"
					Quality = "256kbps"
					//fmt.Println("Unavailable.\n")
				} else {
					needCheck := false

					if Config.GetM3u8Mode == "all" {
						needCheck = true
					} else if Config.GetM3u8Mode == "hires" && contains(meta.Data[0].Relationships.Tracks.Data[0].Attributes.AudioTraits, "hi-res-lossless") {
						needCheck = true
					}
					var EnhancedHls_m3u8 string
					if needCheck {
						EnhancedHls_m3u8, _ = checkM3u8(meta.Data[0].Relationships.Tracks.Data[0].ID, "album")
						if strings.HasSuffix(EnhancedHls_m3u8, ".m3u8") {
							manifest1.Attributes.ExtendedAssetUrls.EnhancedHls = EnhancedHls_m3u8
						}
					}
					_, Quality, err = extractMedia(manifest1.Attributes.ExtendedAssetUrls.EnhancedHls, true)
					if err != nil {
						fmt.Println("Failed to extract quality from manifest.\n", err)
					}
				}
			}
		}
	}
	stringsToJoin := []string{}
	if meta.Data[0].Attributes.IsAppleDigitalMaster || meta.Data[0].Attributes.IsMasteredForItunes {
		if Config.AppleMasterChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.AppleMasterChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "explicit" {
		if Config.ExplicitChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.ExplicitChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "clean" {
		if Config.CleanChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.CleanChoice)
		}
	}
	Tag_string := strings.Join(stringsToJoin, " ")
	var albumFolder string
	if strings.Contains(albumId, "pl.") {
		albumFolder = strings.NewReplacer(
			"{ArtistName}", "Apple Music",
			"{PlaylistName}", LimitString(meta.Data[0].Attributes.Name),
			"{PlaylistId}", albumId,
			"{Quality}", Quality,
			"{Codec}", Codec,
			"{Tag}", Tag_string,
		).Replace(Config.PlaylistFolderFormat)
	} else {
		albumFolder = strings.NewReplacer(
			"{ReleaseDate}", meta.Data[0].Attributes.ReleaseDate,
			"{ReleaseYear}", meta.Data[0].Attributes.ReleaseDate[:4],
			"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
			"{AlbumName}", LimitString(meta.Data[0].Attributes.Name),
			"{UPC}", meta.Data[0].Attributes.Upc,
			"{RecordLabel}", meta.Data[0].Attributes.RecordLabel,
			"{Copyright}", meta.Data[0].Attributes.Copyright,
			"{AlbumId}", albumId,
			"{Quality}", Quality,
			"{Codec}", Codec,
			"{Tag}", Tag_string,
		).Replace(Config.AlbumFolderFormat)
	}
	if strings.HasSuffix(albumFolder, ".") {
		albumFolder = strings.ReplaceAll(albumFolder, ".", "")
	}
	albumFolder = strings.TrimSpace(albumFolder)
	if len(albumFolder) > maxNameLength {
		albumFolder = albumId
	}
	sanAlbumFolder := filepath.Join(singerFolder, forbiddenNames.ReplaceAllString(albumFolder, "_"))
	os.MkdirAll(sanAlbumFolder, os.ModePerm)
	fmt.Println(albumFolder)
	//get artist cover
	if Config.SaveArtistCover && !(strings.Contains(albumId, "pl.")) {
		if len(meta.Data[0].Relationships.Artists.Data) > 0 {
			_, err = writeCover(singerFolder, "folder", meta.Data[0].Relationships.Artists.Data[0].Attributes.Artwork.Url)
			if err != nil {
				fmt.Println("Failed to write artist cover.")
			}
		}
	}
	//get album cover
	covPath, err := writeCover(sanAlbumFolder, "cover", meta.Data[0].Attributes.Artwork.URL)
	if err != nil {
		fmt.Println("Failed to write cover.")
	}
	//get animated artwork
	if Config.SaveAnimatedArtwork && meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video != "" {
		fmt.Println("Found Animation Artwork.")

		// Download square version
		motionvideoUrlSquare, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video)
		if err != nil {
			fmt.Println("no motion video square.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(sanAlbumFolder, "square_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork square exists.")
			}
			if exists {
				fmt.Println("Animated artwork square already exists locally.")
			} else {
				fmt.Println("Animation Artwork Square Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlSquare, "-c", "copy", filepath.Join(sanAlbumFolder, "square_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork square dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Square Downloaded")
				}
			}
		}

		if Config.EmbyAnimatedArtwork {
			// Convert square version to gif
			cmd3 := exec.Command("ffmpeg", "-i", filepath.Join(sanAlbumFolder, "square_animated_artwork.mp4"), "-vf", "scale=440:-1", "-r", "24", "-f", "gif", filepath.Join(sanAlbumFolder, "folder.jpg"))
			if err := cmd3.Run(); err != nil {
				fmt.Printf("animated artwork square to gif err: %v\n", err)
			}
		}

		// Download tall version
		motionvideoUrlTall, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailTall.Video)
		if err != nil {
			fmt.Println("no motion video tall.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(sanAlbumFolder, "tall_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork tall exists.")
			}
			if exists {
				fmt.Println("Animated artwork tall already exists locally.")
			} else {
				fmt.Println("Animation Artwork Tall Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlTall, "-c", "copy", filepath.Join(sanAlbumFolder, "tall_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork tall dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Tall Downloaded")
				}
			}
		}
	}
	trackTotal := len(meta.Data[0].Relationships.Tracks.Data)
	arr := make([]int, trackTotal)
	for i := 0; i < trackTotal; i++ {
		arr[i] = i + 1
	}
	selected := []int{}

	if dl_song {
		if urlArg_i == "" {
			//fmt.Println("URL does not contain parameter 'i'. Please ensure the URL includes 'i' or use another mode.")
			//return nil
		} else {
			for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
				trackNum++
				if urlArg_i == track.ID {
					downloadTrack(trackNum, trackTotal, meta, track, albumId, token, storefront, mediaUserToken, sanAlbumFolder, Codec, covPath)
					return nil
				}
			}
		}
		return nil
	}

	if !dl_select {
		selected = arr
	} else {
		var data [][]string
		for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
			trackNum++
			var trackName string
			if meta.Data[0].Type == "albums" {
				trackName = fmt.Sprintf("%02d. %s", track.Attributes.TrackNumber, track.Attributes.Name)
			} else {
				trackName = fmt.Sprintf("%s - %s", track.Attributes.Name, track.Attributes.ArtistName)
			}
			data = append(data, []string{fmt.Sprint(trackNum),
				trackName,
				track.Attributes.ContentRating,
				track.Type})

		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"", "Track Name", "Rating", "Type"})
		//table.SetFooter([]string{"", "", "Footer", "Footer4"})
		table.SetRowLine(false)
		//table.SetAutoMergeCells(true)
		table.SetCaption(meta.Data[0].Type == "albums", fmt.Sprintf("Storefront: %s, %d tracks missing", strings.ToUpper(storefront), meta.Data[0].Attributes.TrackCount-trackTotal))
		table.SetHeaderColor(tablewriter.Colors{},
			tablewriter.Colors{tablewriter.FgRedColor, tablewriter.Bold},
			tablewriter.Colors{tablewriter.FgBlackColor, tablewriter.Bold},
			tablewriter.Colors{tablewriter.FgBlackColor, tablewriter.Bold})

		table.SetColumnColor(tablewriter.Colors{tablewriter.FgCyanColor},
			tablewriter.Colors{tablewriter.Bold, tablewriter.FgRedColor},
			tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
			tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})
		for _, row := range data {
			if row[2] == "explicit" {
				row[2] = "E"
			} else if row[2] == "clean" {
				row[2] = "C"
			} else {
				row[2] = "None"
			}
			if row[3] == "music-videos" {
				row[3] = "MV"
			} else if row[3] == "songs" {
				row[3] = "SONG"
			}
			table.Append(row)
		}
		//table.AppendBulk(data)
		table.Render()
		fmt.Println("Please select from the track options above (multiple options separated by commas, ranges supported, or type 'all' to select all)")
		cyanColor := color.New(color.FgCyan)
		cyanColor.Print("select: ")
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println(err)
		}
		input = strings.TrimSpace(input)
		if input == "all" {
			fmt.Println("You have selected all options:")
			selected = arr
		} else {
			selectedOptions := [][]string{}
			parts := strings.Split(input, ",")
			for _, part := range parts {
				if strings.Contains(part, "-") { // Range setting
					rangeParts := strings.Split(part, "-")
					selectedOptions = append(selectedOptions, rangeParts)
				} else { // Single option
					selectedOptions = append(selectedOptions, []string{part})
				}
			}
			//
			for _, opt := range selectedOptions {
				if len(opt) == 1 { // Single option
					num, err := strconv.Atoi(opt[0])
					if err != nil {
						fmt.Println("Invalid option:", opt[0])
						continue
					}
					if num > 0 && num <= len(arr) {
						selected = append(selected, num)
						//args = append(args, urls[num-1])
					} else {
						fmt.Println("Option out of range:", opt[0])
					}
				} else if len(opt) == 2 { // Range
					start, err1 := strconv.Atoi(opt[0])
					end, err2 := strconv.Atoi(opt[1])
					if err1 != nil || err2 != nil {
						fmt.Println("Invalid range:", opt)
						continue
					}
					if start < 1 || end > len(arr) || start > end {
						fmt.Println("Range out of range:", opt)
						continue
					}
					for i := start; i <= end; i++ {
						//fmt.Println(options[i-1])
						selected = append(selected, i)
					}
				} else {
					fmt.Println("Invalid option:", opt)
				}
			}
		}
		fmt.Println("Selected options:", selected)
	}
	for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
		trackNum++
		if isInArray(okDict[albumId], trackNum) {
			//fmt.Println("已完成直接跳过.\n")
			counter.Total++
			counter.Success++
			continue
		}
		if isInArray(selected, trackNum) {
			downloadTrack(trackNum, trackTotal, meta, track, albumId, token, storefront, mediaUserToken, sanAlbumFolder, Codec, covPath)
		}
	}
	return nil
}

func writeMP4Tags(trackPath, lrc string, meta *structs.AutoGenerated, trackNum, trackTotal int) error {
	index := trackNum - 1

	t := &mp4tag.MP4Tags{
		Title:      meta.Data[0].Relationships.Tracks.Data[index].Attributes.Name,
		TitleSort:  meta.Data[0].Relationships.Tracks.Data[index].Attributes.Name,
		Artist:     meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName,
		ArtistSort: meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName,
		Custom: map[string]string{
			"PERFORMER":   meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName,
			"RELEASETIME": meta.Data[0].Relationships.Tracks.Data[index].Attributes.ReleaseDate,
			"ISRC":        meta.Data[0].Relationships.Tracks.Data[index].Attributes.Isrc,
			"LABEL":       meta.Data[0].Attributes.RecordLabel,
			"UPC":         meta.Data[0].Attributes.Upc,
		},
		Composer:     meta.Data[0].Relationships.Tracks.Data[index].Attributes.ComposerName,
		ComposerSort: meta.Data[0].Relationships.Tracks.Data[index].Attributes.ComposerName,
		Date:         meta.Data[0].Attributes.ReleaseDate,
		CustomGenre:  meta.Data[0].Relationships.Tracks.Data[index].Attributes.GenreNames[0],
		Copyright:    meta.Data[0].Attributes.Copyright,
		Publisher:    meta.Data[0].Attributes.RecordLabel,
		Lyrics:       lrc,
	}

	if !strings.Contains(meta.Data[0].ID, "pl.") {
		albumID, err := strconv.ParseUint(meta.Data[0].ID, 10, 32)
		if err != nil {
			return err
		}
		t.ItunesAlbumID = int32(albumID)
	}

	if len(meta.Data[0].Relationships.Artists.Data) > 0 {
		if len(meta.Data[0].Relationships.Tracks.Data[index].Relationships.Artists.Data) > 0 {
			artistID, err := strconv.ParseUint(meta.Data[0].Relationships.Tracks.Data[index].Relationships.Artists.Data[0].ID, 10, 32)
			if err != nil {
				return err
			}
			t.ItunesArtistID = int32(artistID)
		}
	}

	if strings.Contains(meta.Data[0].ID, "pl.") && !Config.UseSongInfoForPlaylist {
		t.DiscNumber = 1
		t.DiscTotal = 1
		t.TrackNumber = int16(trackNum)
		t.TrackTotal = int16(trackTotal)
		t.Album = meta.Data[0].Attributes.Name
		t.AlbumSort = meta.Data[0].Attributes.Name
		t.AlbumArtist = meta.Data[0].Attributes.ArtistName
		t.AlbumArtistSort = meta.Data[0].Attributes.ArtistName
	} else if strings.Contains(meta.Data[0].ID, "pl.") && Config.UseSongInfoForPlaylist {
		t.DiscNumber = int16(meta.Data[0].Relationships.Tracks.Data[index].Attributes.DiscNumber)
		t.DiscTotal = int16(meta.Data[0].Relationships.Tracks.Data[trackTotal-1].Attributes.DiscNumber)
		t.TrackNumber = int16(meta.Data[0].Relationships.Tracks.Data[index].Attributes.TrackNumber)
		t.TrackTotal = int16(trackTotal)
		t.Album = meta.Data[0].Relationships.Tracks.Data[index].Attributes.AlbumName
		t.AlbumSort = meta.Data[0].Relationships.Tracks.Data[index].Attributes.AlbumName
		t.AlbumArtist = meta.Data[0].Relationships.Tracks.Data[index].Relationships.Albums.Data[0].Attributes.ArtistName
		t.AlbumArtistSort = meta.Data[0].Relationships.Tracks.Data[index].Relationships.Albums.Data[0].Attributes.ArtistName
	} else {
		t.DiscNumber = int16(meta.Data[0].Relationships.Tracks.Data[index].Attributes.DiscNumber)
		t.DiscTotal = int16(meta.Data[0].Relationships.Tracks.Data[trackTotal-1].Attributes.DiscNumber)
		t.TrackNumber = int16(meta.Data[0].Relationships.Tracks.Data[index].Attributes.TrackNumber)
		t.TrackTotal = int16(trackTotal)
		t.Album = meta.Data[0].Relationships.Tracks.Data[index].Attributes.AlbumName
		t.AlbumSort = meta.Data[0].Relationships.Tracks.Data[index].Attributes.AlbumName
		t.AlbumArtist = meta.Data[0].Attributes.ArtistName
		t.AlbumArtistSort = meta.Data[0].Attributes.ArtistName
	}

	if meta.Data[0].Relationships.Tracks.Data[index].Attributes.ContentRating == "explicit" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryExplicit
	} else if meta.Data[0].Relationships.Tracks.Data[index].Attributes.ContentRating == "clean" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryClean
	} else {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryNone
	}

	// Add Album URL and Artist URL to custom tags
	if len(meta.Data[0].Relationships.Tracks.Data[index].Relationships.Albums.Data) > 0 {
		albumURL := meta.Data[0].Relationships.Tracks.Data[index].Relationships.Albums.Data[0].Attributes.URL
		t.Custom["ALBUM_URL"] = albumURL

		if len(meta.Data[0].Relationships.Tracks.Data[index].Relationships.Artists.Data) > 0 {
			artistID := meta.Data[0].Relationships.Tracks.Data[index].Relationships.Artists.Data[0].ID
			storefront, err := getStorefrontFromURL(albumURL)
			if err == nil {
				artistURL := fmt.Sprintf("https://music.apple.com/%s/artist/%s", storefront, artistID)
				t.Custom["ARTIST_URL"] = artistURL
			}
		}
	}

	mp4, err := mp4tag.Open(trackPath)
	if err != nil {
		return err
	}
	defer mp4.Close()
	err = mp4.Write(t, []string{})
	if err != nil {
		return err
	}
	return nil
}

func main() {
	err := loadConfig()
	if err != nil {
		fmt.Printf("load Config failed: %v", err)
		return
	}
	token, err := getToken()
	if err != nil {
		if Config.AuthorizationToken != "" && Config.AuthorizationToken != "your-authorization-token" {
			token = strings.Replace(Config.AuthorizationToken, "Bearer ", "", -1)
		} else {
			fmt.Println("Failed to get token.")
			return
		}
	}
	// Define command-line flags
	pflag.BoolVar(&dl_atmos, "atmos", false, "Enable atmos download mode")
	pflag.BoolVar(&dl_aac, "aac", false, "Enable adm-aac download mode")
	pflag.BoolVar(&dl_select, "select", false, "Enable selective download")
	pflag.BoolVar(&dl_song, "song", false, "Enable single song download mode")
	pflag.BoolVar(&artist_select, "all-album", false, "Download all artist albums")
	pflag.BoolVar(&debug_mode, "debug", false, "Enable debug mode to show audio quality information")
	pflag.BoolVar(&lyrics_only, "lyrics-only", false, "Download only lyrics, skip audio/video")
	pflag.BoolVar(&atmos_only, "atmos-only", false, "Download only albums with Atmos tracks, skip others")
	pflag.BoolVar(&skip_mv, "skip-mv", false, "Skip all music video downloads")
	pflag.BoolVar(&cover_art_only, "cover-art", false, "Download only cover art")
	alac_max = pflag.Int("alac-max", Config.AlacMax, "Specify the max quality for download alac")
	atmos_max = pflag.Int("atmos-max", Config.AtmosMax, "Specify the max quality for download atmos")
	aac_type = pflag.String("aac-type", Config.AacType, "Select AAC type, aac aac-binaural aac-downmix")
	mv_audio_type = pflag.String("mv-audio-type", Config.MVAudioType, "Select MV audio type, atmos ac3 aac")
	mv_max = pflag.Int("mv-max", Config.MVMax, "Specify the max quality for download MV")

	// Custom usage message for help
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] url1 url2 ...\n", "[main | main.exe | go run main.go]")
		fmt.Println("Options:")
		pflag.PrintDefaults()
	}

	// Parse the flag arguments
	pflag.Parse()
	Config.AlacMax = *alac_max
	Config.AtmosMax = *atmos_max
	Config.AacType = *aac_type
	Config.MVAudioType = *mv_audio_type
	Config.MVMax = *mv_max

	// If atmos_only is enabled, set dl_atmos to true
	if atmos_only {
		dl_atmos = true
		fmt.Println("Atmos-only mode enabled: Will only download albums with Dolby Atmos tracks")
	}

	// Ensure SaveLrcFile is enabled when lyrics_only is true
	if lyrics_only && !Config.SaveLrcFile {
		Config.SaveLrcFile = true
		fmt.Println("Enabled SaveLrcFile configuration for lyrics-only mode")
	}

	args := pflag.Args()
	if len(args) == 0 {
		fmt.Println("No URLs provided. Please provide at least one URL.")
		pflag.Usage()
		return
	}
	os.Args = args

	// Handle artist URL with cover-art-only mode
	if strings.Contains(os.Args[0], "/artist/") && cover_art_only {
		fmt.Println("Artist cover art download mode enabled")
		fmt.Printf("Processing artist URL: %s\n", os.Args[0])

		// Download artist cover first
		err := downloadArtistCover(os.Args[0], token)
		if err != nil {
			fmt.Printf("⚠️ Failed to download artist cover image: %v\n", err)
			fmt.Println("Will continue with album covers...")
		} else {
			fmt.Println("✓ Artist cover image downloaded successfully")
		}

		// Continue with album covers
		fmt.Println("Retrieving artist information...")
		urlArtistName, urlArtistID, err := getUrlArtistName(os.Args[0], token)
		if err != nil {
			fmt.Printf("❌ Error: Failed to get artist information: %v\n", err)
			fmt.Println("Cannot continue without artist information.")
			return
		}
		fmt.Printf("Found artist: %s (ID: %s)\n", urlArtistName, urlArtistID)

		Config.ArtistFolderFormat = strings.NewReplacer(
			"{UrlArtistName}", LimitString(urlArtistName),
			"{ArtistId}", urlArtistID,
		).Replace(Config.ArtistFolderFormat)

		fmt.Println("Retrieving artist albums...")
		albumArgs, err := checkArtist(os.Args[0], token, "albums")
		if err != nil {
			fmt.Printf("❌ Error: Failed to get artist albums: %v\n", err)
			fmt.Println("Cannot continue without album information.")
			return
		}

		if len(albumArgs) == 0 {
			fmt.Println("⚠️ Warning: No albums found for this artist")
			fmt.Println("Nothing to download.")
			return
		}

		fmt.Printf("Found %d albums\n", len(albumArgs))

		// Skip MV if we're only downloading cover art
		os.Args = albumArgs
		fmt.Println("Proceeding to download album covers...")
	} else if strings.Contains(os.Args[0], "/artist/") {
		// Original artist URL handling code
		urlArtistName, urlArtistID, err := getUrlArtistName(os.Args[0], token)
		if err != nil {
			fmt.Println("Failed to get artistname.")
			return
		}
		//fmt.Println("get artistname:", urlArtistName)
		Config.ArtistFolderFormat = strings.NewReplacer(
			"{UrlArtistName}", LimitString(urlArtistName),
			"{ArtistId}", urlArtistID,
		).Replace(Config.ArtistFolderFormat)
		albumArgs, err := checkArtist(os.Args[0], token, "albums")
		if err != nil {
			fmt.Println("Failed to get artist albums.")
			return
		}
		mvArgs, err := checkArtist(os.Args[0], token, "music-videos")
		if err != nil {
			fmt.Println("Failed to get artist music-videos.")
			//return
		}
		os.Args = append(albumArgs, mvArgs...)
	}
	albumTotal := len(os.Args)
	for {
		for albumNum, urlRaw := range os.Args {
			fmt.Printf("Album %d of %d:\n", albumNum+1, albumTotal)
			var storefront, albumId string
			//mv dl dev
			if strings.Contains(urlRaw, "/music-video/") {
				if debug_mode {
					continue
				}
				counter.Total++

				// Skip if the skip_mv flag is enabled
				if skip_mv {
					fmt.Println("Skipping music video (skip-mv enabled)")
					counter.Success++
					continue
				}

				if len(Config.MediaUserToken) <= 50 {
					fmt.Println("meida-user-token is not set, skip MV dl")
					counter.Success++
					continue
				}
				if _, err := exec.LookPath("mp4decrypt"); err != nil {
					fmt.Println("mp4decrypt is not found, skip MV dl")
					counter.Success++
					continue
				}
				mvSaveDir := strings.NewReplacer(
					"{ArtistName}", "",
					"{UrlArtistName}", "",
					"{ArtistId}", "",
				).Replace(Config.ArtistFolderFormat)
				if mvSaveDir != "" {
					mvSaveDir = filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(mvSaveDir, "_"))
				} else {
					mvSaveDir = Config.AlacSaveFolder
				}
				storefront, albumId = checkUrlMv(urlRaw)
				err := mvDownloader(albumId, mvSaveDir, token, storefront, Config.MediaUserToken, nil)
				if err != nil {
					fmt.Println("\u26A0 Failed to dl MV:", err)
					counter.Error++
					continue
				}
				counter.Success++
				continue
			}
			if strings.Contains(urlRaw, "/song/") {
				urlRaw, err = getUrlSong(urlRaw, token)
				dl_song = true
				if err != nil {
					fmt.Println("Failed to get Song info.")
				}
			}
			if strings.Contains(urlRaw, "/playlist/") {
				storefront, albumId = checkUrlPlaylist(urlRaw)
			} else {
				storefront, albumId = checkUrl(urlRaw)
			}
			if albumId == "" {
				fmt.Printf("Invalid URL: %s\n", urlRaw)
				continue
			}
			parse, err := url.Parse(urlRaw)
			if err != nil {
				log.Fatalf("Invalid URL: %v", err)
			}
			var urlArg_i = parse.Query().Get("i")
			err = rip(albumId, token, storefront, Config.MediaUserToken, urlArg_i)
			if err != nil {
				fmt.Println("Album failed.")
				fmt.Println(err)
			}
		}
		fmt.Printf("=======  [\u2714 ] Completed: %d/%d  |  [\u26A0 ] Warnings: %d  |  [\u2716 ] Errors: %d  =======\n", counter.Success, counter.Total, counter.Unavailable+counter.NotSong, counter.Error)
		if counter.Error == 0 {
			break
		}
		fmt.Println("Error detected, press Enter to try again...")
		fmt.Scanln()
		fmt.Println("Start trying again...")
		counter = structs.Counter{}
	}
}
func mvDownloader(adamID string, saveDir string, token string, storefront string, mediaUserToken string, meta *structs.AutoGenerated) error {
	// Skip MV download in lyrics-only mode or if skip-mv is enabled
	if lyrics_only {
		fmt.Println("Skipping MV download (lyrics-only mode)")
		return nil
	}

	// Skip if skip_mv flag is enabled
	if skip_mv {
		fmt.Println("Skipping MV download (skip-mv enabled)")
		return nil
	}

	// Skip MV download in cover-art-only mode
	if cover_art_only {
		fmt.Println("Skipping MV download (cover-art-only mode)")
		return nil
	}

	MVInfo, err := getMVInfoFromAdam(adamID, token, storefront)
	if err != nil {
		fmt.Println("\u26A0 Failed to get MV manifest:", err)
		return nil
	}

	// For single music video downloads, set saveDir to artist's folder
	if meta == nil {
		artistName := MVInfo.Data[0].Attributes.ArtistName
		artistId := "" // Relationships field is not available, so artistId is empty
		singerFoldername := strings.NewReplacer(
			"{ArtistName}", LimitString(artistName),
			"{ArtistId}", artistId,
			"{UrlArtistName}", LimitString(artistName),
		).Replace(Config.ArtistFolderFormat)
		if strings.HasSuffix(singerFoldername, ".") {
			singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
		}
		singerFoldername = strings.TrimSpace(singerFoldername)
		if len(singerFoldername) > maxNameLength {
			singerFoldername = "UnknownArtist" // Fallback if name is too long and no artistId
		}
		saveDir = filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
		os.MkdirAll(saveDir, os.ModePerm)
	}

	// Existing code continues here...
	var trackTotal int
	var trackNum int
	var index int
	if meta != nil {
		trackTotal = len(meta.Data[0].Relationships.Tracks.Data)
		for i, track := range meta.Data[0].Relationships.Tracks.Data {
			if adamID == track.ID {
				index = i
				trackNum = i + 1
			}
		}
	}

	if strings.HasSuffix(saveDir, ".") {
		saveDir = strings.ReplaceAll(saveDir, ".", "")
	}
	saveDir = strings.TrimSpace(saveDir)

	vidPath := filepath.Join(saveDir, fmt.Sprintf("%s_vid.mp4", adamID))
	audPath := filepath.Join(saveDir, fmt.Sprintf("%s_aud.mp4", adamID))
	mvSaveName := fmt.Sprintf("%s (%s)", MVInfo.Data[0].Attributes.Name, adamID)
	if meta != nil {
		mvSaveName = fmt.Sprintf("%02d. %s", trackNum, MVInfo.Data[0].Attributes.Name)
	}
	mvFileName := fmt.Sprintf("%s.mp4", forbiddenNames.ReplaceAllString(mvSaveName, "_"))
	if len(mvFileName) > maxNameLength {
		mvFileName = fmt.Sprintf("%s.mp4", adamID)
	}
	mvOutPath := filepath.Join(saveDir, mvFileName)

	fmt.Println(MVInfo.Data[0].Attributes.Name)

	exists, _ := fileExists(mvOutPath)
	if exists {
		fmt.Println("MV already exists locally.")
		return nil
	}

	mvm3u8url, _, _ := runv3.GetWebplayback(adamID, token, mediaUserToken, true)
	if mvm3u8url == "" {
		return errors.New("media-user-token may wrong or expired")
	}

	os.MkdirAll(saveDir, os.ModePerm)
	// Video
	videom3u8url, _ := extractVideo(mvm3u8url)
	videokeyAndUrls, _ := runv3.Run(adamID, videom3u8url, token, mediaUserToken, true)
	_ = runv3.ExtMvData(videokeyAndUrls, vidPath)
	// Audio
	audiom3u8url, _ := extractMvAudio(mvm3u8url)
	audiokeyAndUrls, _ := runv3.Run(adamID, audiom3u8url, token, mediaUserToken, true)
	_ = runv3.ExtMvData(audiokeyAndUrls, audPath)

	// Tags
	tags := []string{
		"tool=",
		fmt.Sprintf("artist=%s", MVInfo.Data[0].Attributes.ArtistName),
		fmt.Sprintf("title=%s", MVInfo.Data[0].Attributes.Name),
		fmt.Sprintf("genre=%s", MVInfo.Data[0].Attributes.GenreNames[0]),
		fmt.Sprintf("created=%s", MVInfo.Data[0].Attributes.ReleaseDate),
		fmt.Sprintf("ISRC=%s", MVInfo.Data[0].Attributes.Isrc),
	}

	if MVInfo.Data[0].Attributes.ContentRating == "explicit" {
		tags = append(tags, "rating=1")
	} else if MVInfo.Data[0].Attributes.ContentRating == "clean" {
		tags = append(tags, "rating=2")
	} else {
		tags = append(tags, "rating=0")
	}

	if meta != nil {
		if meta.Data[0].Type == "playlists" && !Config.UseSongInfoForPlaylist {
			tags = append(tags, "disk=1/1")
			tags = append(tags, fmt.Sprintf("album=%s", meta.Data[0].Attributes.Name))
			tags = append(tags, fmt.Sprintf("track=%d", trackNum))
			tags = append(tags, fmt.Sprintf("tracknum=%d/%d", trackNum, trackTotal))
			tags = append(tags, fmt.Sprintf("album_artist=%s", meta.Data[0].Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("performer=%s", meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("copyright=%s", meta.Data[0].Attributes.Copyright))
			tags = append(tags, fmt.Sprintf("UPC=%s", meta.Data[0].Attributes.Upc))
		} else {
			tags = append(tags, fmt.Sprintf("album=%s", meta.Data[0].Relationships.Tracks.Data[index].Attributes.AlbumName))
			tags = append(tags, fmt.Sprintf("disk=%d/%d", meta.Data[0].Relationships.Tracks.Data[index].Attributes.DiscNumber, meta.Data[0].Relationships.Tracks.Data[trackTotal-1].Attributes.DiscNumber))
			tags = append(tags, fmt.Sprintf("track=%d", meta.Data[0].Relationships.Tracks.Data[index].Attributes.TrackNumber))
			tags = append(tags, fmt.Sprintf("tracknum=%d/%d", meta.Data[0].Relationships.Tracks.Data[index].Attributes.TrackNumber, meta.Data[0].Attributes.TrackCount))
			tags = append(tags, fmt.Sprintf("album_artist=%s", meta.Data[0].Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("performer=%s", meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("copyright=%s", meta.Data[0].Attributes.Copyright))
			tags = append(tags, fmt.Sprintf("UPC=%s", meta.Data[0].Attributes.Upc))
		}
	} else {
		tags = append(tags, fmt.Sprintf("album=%s", MVInfo.Data[0].Attributes.AlbumName))
		tags = append(tags, fmt.Sprintf("disk=%d", MVInfo.Data[0].Attributes.DiscNumber))
		tags = append(tags, fmt.Sprintf("track=%d", MVInfo.Data[0].Attributes.TrackNumber))
		tags = append(tags, fmt.Sprintf("tracknum=%d", MVInfo.Data[0].Attributes.TrackNumber))
		tags = append(tags, fmt.Sprintf("performer=%s", MVInfo.Data[0].Attributes.ArtistName))
	}

	var covPath string
	if true { // Assuming cover embedding is always enabled
		thumbURL := MVInfo.Data[0].Attributes.Artwork.URL
		baseThumbName := forbiddenNames.ReplaceAllString(mvSaveName, "_") + "_thumbnail"
		covPath, err = writeCover(saveDir, baseThumbName, thumbURL)
		if err != nil {
			fmt.Println("Failed to save MV thumbnail:", err)
		} else {
			tags = append(tags, fmt.Sprintf("cover=%s", covPath))
		}
	}

	tagsString := strings.Join(tags, ":")
	muxCmd := exec.Command("MP4Box", "-itags", tagsString, "-quiet", "-add", vidPath, "-add", audPath, "-keep-utc", "-new", mvOutPath)
	fmt.Printf("MV Remuxing...")
	if err := muxCmd.Run(); err != nil {
		fmt.Printf("MV mux failed: %v\n", err)
		return err
	}
	fmt.Printf("\rMV Remuxed.   \n")

	// Add custom MV_ID tag using mp4tag
	mp4, err := mp4tag.Open(mvOutPath)
	if err != nil {
		fmt.Printf("Failed to open MP4 for tagging: %v\n", err)
		return err
	}
	defer mp4.Close()

	t := &mp4tag.MP4Tags{
		Custom: map[string]string{
			"MV_ID": adamID,
		},
	}
	err = mp4.Write(t, []string{})
	if err != nil {
		fmt.Printf("Failed to write custom tags: %v\n", err)
		return err
	}

	// Cleanup temporary files
	defer os.Remove(vidPath)
	defer os.Remove(audPath)
	defer os.Remove(covPath)

	return nil
}

func extractMvAudio(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	audioString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(audioString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	audio := from.(*m3u8.MasterPlaylist)

	var audioPriority = []string{"audio-atmos", "audio-ac3", "audio-stereo-256"}
	if Config.MVAudioType == "ac3" {
		audioPriority = []string{"audio-ac3", "audio-stereo-256"}
	} else if Config.MVAudioType == "aac" {
		audioPriority = []string{"audio-stereo-256"}
	}

	re := regexp.MustCompile(`_gr(\d+)_`)

	type AudioStream struct {
		URL     string
		Rank    int
		GroupID string
	}
	var audioStreams []AudioStream

	for _, variant := range audio.Variants {
		for _, audiov := range variant.Alternatives {
			if audiov.URI != "" {
				for _, priority := range audioPriority {
					if audiov.GroupId == priority {
						matches := re.FindStringSubmatch(audiov.URI)
						if len(matches) == 2 {
							var rank int
							fmt.Sscanf(matches[1], "%d", &rank)
							streamUrl, _ := MediaUrl.Parse(audiov.URI)
							audioStreams = append(audioStreams, AudioStream{
								URL:     streamUrl.String(),
								Rank:    rank,
								GroupID: audiov.GroupId,
							})
						}
					}
				}
			}
		}
	}

	if len(audioStreams) == 0 {
		return "", errors.New("no suitable audio stream found")
	}

	sort.Slice(audioStreams, func(i, j int) bool {
		return audioStreams[i].Rank > audioStreams[j].Rank
	})
	fmt.Println("Audio: " + audioStreams[0].GroupID)
	return audioStreams[0].URL, nil
}

func checkM3u8(b string, f string) (string, error) {
	var EnhancedHls string
	if Config.GetM3u8FromDevice {
		adamID := b
		conn, err := net.Dial("tcp", Config.GetM3u8Port)
		if err != nil {
			fmt.Println("Error connecting to device:", err)
			return "none", err
		}
		defer conn.Close()
		if f == "song" {
			fmt.Println("Connected to device")
		}

		// Send the length of adamID and the adamID itself
		adamIDBuffer := []byte(adamID)
		lengthBuffer := []byte{byte(len(adamIDBuffer))}

		// Write length and adamID to the connection
		_, err = conn.Write(lengthBuffer)
		if err != nil {
			fmt.Println("Error writing length to device:", err)
			return "none", err
		}

		_, err = conn.Write(adamIDBuffer)
		if err != nil {
			fmt.Println("Error writing adamID to device:", err)
			return "none", err
		}

		// Read the response (URL) from the device
		response, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			fmt.Println("Error reading response from device:", err)
			return "none", err
		}

		// Trim any newline characters from the response

		response = bytes.TrimSpace(response)
		if len(response) > 0 {
			if f == "song" {
				fmt.Println("Received URL:", string(response))
			}
			EnhancedHls = string(response)
		} else {
			fmt.Println("Received an empty response")
		}
	}
	return EnhancedHls, nil
}

func formatAvailability(available bool, quality string) string {
	if !available {
		return "Not Available"
	}
	return quality
}

func extractMedia(b string, more_mode bool) (string, string, error) {
	masterUrl, err := url.Parse(b)
	if err != nil {
		return "", "", err
	}
	resp, err := http.Get(b)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", "", errors.New("m3u8 not of master type")
	}
	master := from.(*m3u8.MasterPlaylist)
	var streamUrl *url.URL
	sort.Slice(master.Variants, func(i, j int) bool {
		return master.Variants[i].AverageBandwidth > master.Variants[j].AverageBandwidth
	})
	if debug_mode && more_mode {
		fmt.Println("\nDebug: All Available Variants:")
		var data [][]string
		for _, variant := range master.Variants {
			data = append(data, []string{variant.Codecs, variant.Audio, fmt.Sprint(variant.Bandwidth)})
			//fmt.Printf("Codec: %s, Audio: %s, Bandwidth: %d\n",
			//variant.Codecs, variant.Audio, variant.Bandwidth)
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Codec", "Audio", "Bandwidth"})
		//table.SetFooter([]string{"", "", "Total", "$146.93"})
		table.SetAutoMergeCells(true)
		//table.SetAutoMergeCellsByColumnIndex([]int{1,2,3})
		table.SetRowLine(true)
		table.AppendBulk(data)
		table.Render()

		var hasAAC, hasLossless, hasHiRes, hasAtmos, hasDolbyAudio bool
		var aacQuality, losslessQuality, hiResQuality, atmosQuality, dolbyAudioQuality string

		// Check for all formats
		for _, variant := range master.Variants {
			if variant.Codecs == "mp4a.40.2" { // AAC
				hasAAC = true
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitrate, _ := strconv.Atoi(split[2])
					currentBitrate := 0
					if aacQuality != "" {
						current := strings.Split(aacQuality, " | ")[2]
						current = strings.Split(current, " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						aacQuality = fmt.Sprintf("AAC | 2 Channel | %d kbps", bitrate)
					}
				}
			} else if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") { // Dolby Atmos
				hasAtmos = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrateStr := split[len(split)-1]
					// Remove leading "2" if present in "2768"
					if len(bitrateStr) == 4 && bitrateStr[0] == '2' {
						bitrateStr = bitrateStr[1:]
					}
					bitrate, _ := strconv.Atoi(bitrateStr)
					currentBitrate := 0
					if atmosQuality != "" {
						current := strings.Split(strings.Split(atmosQuality, " | ")[2], " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						atmosQuality = fmt.Sprintf("E-AC-3 | 16 Channel | %d kbps", bitrate)
					}
				}
			} else if variant.Codecs == "alac" { // ALAC (Lossless or Hi-Res)
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitDepth := split[len(split)-1]
					sampleRate := split[len(split)-2]
					sampleRateInt, _ := strconv.Atoi(sampleRate)
					if sampleRateInt > 48000 { // Hi-Res
						hasHiRes = true
						hiResQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					} else { // Standard Lossless
						hasLossless = true
						losslessQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					}
				}
			} else if variant.Codecs == "ac-3" { // Dolby Audio
				hasDolbyAudio = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrate, _ := strconv.Atoi(split[len(split)-1])
					dolbyAudioQuality = fmt.Sprintf("AC-3 |  16 Channel | %d kbps", bitrate)
				}
			}
		}

		fmt.Println("Available Audio Formats:")
		fmt.Println("------------------------")
		fmt.Printf("AAC             : %s\n", formatAvailability(hasAAC, aacQuality))
		fmt.Printf("Lossless        : %s\n", formatAvailability(hasLossless, losslessQuality))
		fmt.Printf("Hi-Res Lossless : %s\n", formatAvailability(hasHiRes, hiResQuality))
		fmt.Printf("Dolby Atmos     : %s\n", formatAvailability(hasAtmos, atmosQuality))
		fmt.Printf("Dolby Audio     : %s\n", formatAvailability(hasDolbyAudio, dolbyAudioQuality))
		fmt.Println("------------------------")

		return "", "", nil
	}
	var Quality string
	for _, variant := range master.Variants {
		if dl_atmos {
			if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Atmos variant - %s (Bitrate: %d kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-1])
				if err != nil {
					return "", "", err
				}
				if length_int <= Config.AtmosMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						return "", "", err
					}
					streamUrl = streamUrlTemp
					Quality = fmt.Sprintf("%s kbps", split[len(split)-1])
					break
				}
			} else if variant.Codecs == "ac-3" { // Add Dolby Audio support
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Audio variant - %s (Bitrate: %d kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				streamUrlTemp, err := masterUrl.Parse(variant.URI)
				if err != nil {
					return "", "", err
				}
				streamUrl = streamUrlTemp
				split := strings.Split(variant.Audio, "-")
				Quality = fmt.Sprintf("%s kbps", split[len(split)-1])
				break
			}
		} else if dl_aac {
			if variant.Codecs == "mp4a.40.2" {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found AAC variant - %s (Bitrate: %d)\n", variant.Audio, variant.Bandwidth)
				}
				aacregex := regexp.MustCompile(`audio-stereo-\d+`)
				replaced := aacregex.ReplaceAllString(variant.Audio, "aac")
				if replaced == Config.AacType {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						panic(err)
					}
					streamUrl = streamUrlTemp
					split := strings.Split(variant.Audio, "-")
					Quality = fmt.Sprintf("%s kbps", split[2])
					break
				}
			}
		} else {
			if variant.Codecs == "alac" {
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-2])
				if err != nil {
					return "", "", err
				}
				if length_int <= Config.AlacMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s-bit / %s Hz\n", split[length-1], split[length-2])
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						panic(err)
					}
					streamUrl = streamUrlTemp
					KHZ := float64(length_int) / 1000.0
					Quality = fmt.Sprintf("%sB-%.1fkHz", split[length-1], KHZ)
					break
				}
			}
		}
	}
	if streamUrl == nil {
		return "", "", errors.New("no codec found")
	}
	return streamUrl.String(), Quality, nil
}
func extractVideo(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	videoString := string(body)

	from, listType, err := m3u8.DecodeFrom(strings.NewReader(videoString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	video := from.(*m3u8.MasterPlaylist)

	re := regexp.MustCompile(`_(\d+)x(\d+)`)

	var streamUrl *url.URL
	sort.Slice(video.Variants, func(i, j int) bool {
		return video.Variants[i].AverageBandwidth > video.Variants[j].AverageBandwidth
	})

	maxHeight := Config.MVMax

	for _, variant := range video.Variants {
		matches := re.FindStringSubmatch(variant.URI)
		if len(matches) == 3 {
			height := matches[2]
			var h int
			_, err := fmt.Sscanf(height, "%d", &h)
			if err != nil {
				continue
			}
			if h <= maxHeight {
				streamUrl, err = MediaUrl.Parse(variant.URI)
				if err != nil {
					return "", err
				}
				fmt.Println("Video: " + variant.Resolution + "-" + variant.VideoRange)
				break
			}
		}
	}

	if streamUrl == nil {
		return "", errors.New("no suitable video stream found")
	}

	return streamUrl.String(), nil
}

func getInfoFromAdam(adamId string, token string, storefront string) (*structs.SongData, error) {
	request, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s", storefront, adamId), nil)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("extend", "extendedAssetUrls")
	query.Set("include", "albums")
	query.Set("l", Config.Language)
	request.URL.RawQuery = query.Encode()

	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	request.Header.Set("User-Agent", "iTunes/12.11.3 (Windows; Microsoft Windows 10 x64 Professional Edition (Build 19041); x64) AppleWebKit/7611.1022.4001.1 (dt:2)")
	request.Header.Set("Origin", "https://music.apple.com")

	do, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return nil, errors.New(do.Status)
	}

	obj := new(structs.ApiResult)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return nil, err
	}

	for _, d := range obj.Data {
		if d.ID == adamId {
			return &d, nil
		}
	}
	return nil, nil
}

func getMVInfoFromAdam(adamId string, token string, storefront string) (*structs.AutoGeneratedMusicVideo, error) {
	request, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/music-videos/%s", storefront, adamId), nil)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("l", Config.Language)
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	request.Header.Set("Origin", "https://music.apple.com")

	do, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return nil, errors.New(do.Status)
	}

	obj := new(structs.AutoGeneratedMusicVideo)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func getToken() (string, error) {
	req, err := http.NewRequest("GET", "https://beta.music.apple.com", nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index-legacy-[^/]+\.js`)
	indexJsUri := regex.FindString(string(body))

	req, err = http.NewRequest("GET", "https://beta.music.apple.com"+indexJsUri, nil)
	if err != nil {
		return "", err
	}

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJh([^"]*)`)
	token := regex.FindString(string(body))

	return token, nil
}

// New function to download artist cover image
func downloadArtistCover(artistUrl string, token string) error {
	storefront, artistId := checkUrlArtist(artistUrl)
	if artistId == "" {
		return errors.New("Invalid artist URL: Could not extract artist ID from the URL")
	}
	if storefront == "" {
		return errors.New("Invalid artist URL: Could not extract storefront from the URL")
	}

	fmt.Printf("Attempting to download cover for artist ID: %s from storefront: %s\n", artistId, storefront)

	// Get artist information
	apiUrl := fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s", storefront, artistId)
	req, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return fmt.Errorf("Error creating API request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Origin", "https://music.apple.com")

	// Set query parameters
	query := url.Values{}
	query.Set("l", Config.Language)
	query.Set("fields[artists]", "name,artwork")
	req.URL.RawQuery = query.Encode()

	// Make the request
	fmt.Println("Making API request to fetch artist data...")
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Network error while fetching artist data: %w", err)
	}
	defer do.Body.Close()

	// Check HTTP status
	if do.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned non-200 status: %s (HTTP %d)", do.Status, do.StatusCode)
	}

	// Decode response
	obj := new(structs.AutoGeneratedArtist)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return fmt.Errorf("Error parsing artist data from API: %w", err)
	}

	// Check if we got data
	if len(obj.Data) == 0 {
		return errors.New("API returned empty data array: No artist data found")
	}

	// Check if artist attributes exist
	if obj.Data[0].Attributes.Name == "" {
		return errors.New("Artist name not found in API response")
	}

	// Check if artwork exists
	if obj.Data[0].Attributes.Artwork.URL == "" {
		return errors.New("Artist has no artwork URL in API response")
	}

	// Create artist folder
	artistName := obj.Data[0].Attributes.Name
	fmt.Printf("Found artist: %s (ID: %s)\n", artistName, obj.Data[0].ID)

	singerFoldername := strings.NewReplacer(
		"{UrlArtistName}", LimitString(artistName),
		"{ArtistName}", LimitString(artistName),
		"{ArtistId}", obj.Data[0].ID,
	).Replace(Config.ArtistFolderFormat)

	if strings.HasSuffix(singerFoldername, ".") {
		singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
	}
	singerFoldername = strings.TrimSpace(singerFoldername)
	if len(singerFoldername) > maxNameLength {
		singerFoldername = obj.Data[0].ID
		fmt.Printf("Artist folder name exceeded max length, using ID instead: %s\n", singerFoldername)
	}

	singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	err = os.MkdirAll(singerFolder, os.ModePerm)
	if err != nil {
		return fmt.Errorf("Failed to create artist folder: %w", err)
	}

	// Download artist cover
	fmt.Printf("Downloading artist cover image from URL: %s\n", obj.Data[0].Attributes.Artwork.URL)
	artworkURL := obj.Data[0].Attributes.Artwork.URL
	_, err = writeCover(singerFolder, "folder", artworkURL)
	if err != nil {
		return fmt.Errorf("Failed to download artist cover: %w", err)
	}

	fmt.Println("Artist cover image successfully downloaded!")
	counter.Success++

	return nil
}
