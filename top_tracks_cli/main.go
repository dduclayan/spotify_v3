/*
top_tracks_cli is a CLI tool that generates three playlists of the user's recent top tracks.

Usage:

	main.exe playlist --fill      // Fills up the 'Favorite * Term Tracks' playlists
	main.exe playlist --purge_fav // Purges songs from the 'Favorite * Term Tracks' playlists
	main.exe playlist --list_all  // Lists all the user's playlists

From the test-branch.
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/cenkalti/backoff"
	"github.com/zmb3/spotify/v2"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sync"

	spotifyauth "github.com/zmb3/spotify/v2/auth"
	time2 "time"
)

// redirectURI is the OAuth redirect URI for the application.
// You must register an application at Spotify's developer portal
// and enter this value.
const redirectURI = "http://localhost:8080/callback"

var (
	clientID     = os.Getenv("spotify_clientID")
	clientSecret = os.Getenv("spotify_secret")
	state        = os.Getenv("spotify_state")
	auth         = spotifyauth.New(
		spotifyauth.WithRedirectURL(redirectURI),
		spotifyauth.WithScopes(
			spotifyauth.ScopeUserReadPrivate,
			spotifyauth.ScopeUserTopRead,
			spotifyauth.ScopePlaylistModifyPrivate,
			spotifyauth.ScopePlaylistReadPrivate,
		),
		spotifyauth.WithClientSecret(clientSecret),
		spotifyauth.WithClientID(clientID),
	)
	ch = make(chan *spotify.Client)

	// regex
	shortTermRe = regexp.MustCompile("^Favorite Short Term Tracks$")
	medTermRe   = regexp.MustCompile("^Favorite Medium Term Tracks$")
	longTermRe  = regexp.MustCompile("^Favorite Long Term Tracks$")
	plMatch     = regexp.MustCompile("^Favorite (Short|Medium|Long) Term Tracks$")

	// command flags
	playlistCmd            = flag.NewFlagSet("playlist", flag.ExitOnError)
	playlistList           = playlistCmd.Bool("list_all", false, "list all playlists for current user")
	playlistPurgeFavTracks = playlistCmd.Bool("purge_fav", false, "purge all tracks in \"Favorite short/med/long Term Tracks\"")
	playlistFill           = playlistCmd.Bool("fill", false, "fill playlists with favorite tracks")
)

type playlistConfig struct {
	name          string
	public        bool
	description   string
	collaborative bool
	duration      spotify.Range
	user          *spotify.PrivateUser
	id            spotify.ID
}

func (config *playlistConfig) getTopTracks(ctx context.Context, c *spotify.Client) (*spotify.FullTrackPage, error) {
	tracks, err := c.CurrentUsersTopTracks(ctx, spotify.Timerange(config.duration), spotify.Limit(50))
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve users top tracks: %v", err)
	}
	if tracks == nil {
		fmt.Printf("tracks returned nil for some reason: %v", tracks)
		return nil, nil
	}
	return tracks, nil
}

func (config *playlistConfig) createPlaylist(ctx context.Context, c *spotify.Client, page *spotify.FullTrackPage) error {
	newPlaylist, err := c.CreatePlaylistForUser(ctx, config.user.ID, config.name, config.description, config.public, config.collaborative)
	if err != nil {
		return err
	}
	for _, v := range page.Tracks {
		_, err := c.AddTracksToPlaylist(ctx, newPlaylist.ID, v.ID)
		if err != nil {
			return fmt.Errorf("AddTracksToPlaylist(): unable to add track: %v\n", err)
		}
	}
	return nil
}

func fillPlaylist(ctx context.Context, c *spotify.Client, playlistID spotify.ID, page *spotify.FullTrackPage) error {
	for _, track := range page.Tracks {
		op := func() error {
			_, err := c.AddTracksToPlaylist(ctx, playlistID, track.ID)
			if err != nil {
				return fmt.Errorf("c.AddTracksToPlaylist(ctx,%v,%v): %v", playlistID, track.ID, err)
			}
			return nil
		}

		err := backoff.Retry(op, backoff.NewExponentialBackOff())
		if err != nil {
			return fmt.Errorf("fillPlaylist(ctx,spotifyClient,%v,spotifyFullTrackPage): %v", playlistID, err)
		}
	}
	return nil
}

func purgeTracks(ctx context.Context, c *spotify.Client, playlist spotify.SimplePlaylist) error {
	plTracks, err := c.GetPlaylistItems(ctx, playlist.ID)
	if err != nil {
		return err
	}
	var plTrackIDs []spotify.ID
	for _, v := range plTracks.Items {
		plTrackIDs = append(plTrackIDs, v.Track.Track.ID)
	}
	_, err = c.RemoveTracksFromPlaylist(ctx, playlist.ID, plTrackIDs...)
	return nil
}

func getCurrentPlaylists(ctx context.Context, c *spotify.Client) (*spotify.SimplePlaylistPage, error) {
	pl, err := c.CurrentUsersPlaylists(ctx, spotify.Limit(50))
	if err != nil {
		return nil, err
	}
	return pl, nil
}

// TODO(dduclayan): This should probably be renamed to something else, as it's getting and creating the playlists if
// they are not found.
func getAutomatedPlaylists(ctx context.Context, c *spotify.Client, user *spotify.PrivateUser, playlists *spotify.SimplePlaylistPage) ([]spotify.SimplePlaylist, error) {
	var foundPlaylists []spotify.SimplePlaylist
	for _, v := range playlists.Playlists {
		if plMatch.MatchString(v.Name) {
			foundPlaylists = append(foundPlaylists, v)
		}
	}
	if len(foundPlaylists) == 0 {
		playlistNames := []string{"Favorite Short Term Tracks", "Favorite Medium Term Tracks", "Favorite Long Term Tracks"}
		description := "automated from top_tracks_cli"
		for _, v := range playlistNames {
			pl, err := c.CreatePlaylistForUser(ctx, user.ID, v, description, false, false)
			if err != nil {
				return nil, fmt.Errorf("CreatePlaylistForUser(ctx,%v,%v,%v,false,false): %v", user.ID, v, description, err)
			}
			foundPlaylists = append(foundPlaylists, pl.SimplePlaylist)
		}
	}
	return foundPlaylists, nil
}

func getTopTracksAndFill(ctx context.Context, wg *sync.WaitGroup, c *spotify.Client, p playlistConfig) error {
	defer wg.Done()
	tt, err := p.getTopTracks(ctx, c)
	if err != nil {
		return fmt.Errorf("getTopTracks(): %v\n", err)
	}
	if err = fillPlaylist(ctx, c, p.id, tt); err != nil {
		return fmt.Errorf("fillPlaylist(): %v\n", err)
	}
	return nil
}

func completeAuth(w http.ResponseWriter, r *http.Request) {
	tok, err := auth.Token(r.Context(), state, r)
	if err != nil {
		http.Error(w, "Couldn't get token", http.StatusForbidden)
		log.Fatal(err)
	}
	if st := r.FormValue("state"); st != state {
		http.NotFound(w, r)
		log.Fatalf("State mismatch: %s != %s\n", st, state)
	}

	// use the token to get an authenticated client
	client := spotify.New(auth.Client(r.Context(), tok))
	_, err = fmt.Fprintf(w, "Login Completed!")
	if err != nil {
		fmt.Printf("Fprintf(\"Login Completed\"): %v", err)
		os.Exit(1)
	}
	ch <- client
}

func openBrowser(url string) {
	var err error

	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	flag.Parse()
	start := time2.Now()
	ctx := context.Background()
	http.HandleFunc("/callback", completeAuth)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Got request for:", r.URL.String())
	})
	go func() {
		err := http.ListenAndServe(":8080", nil)
		if err != nil {
			log.Fatal(err)
		}
	}()

	url := auth.AuthURL(state)
	openBrowser(url)

	// wait for auth to complete
	client := <-ch

	// use the client to make calls that require authorization
	user, err := client.CurrentUser(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("You are logged in as:", user.ID)

	switch os.Args[1] {
	case "playlist":
		if err := playlistCmd.Parse(os.Args[2:]); err != nil {
			fmt.Println("couldn't parse os.Args[2:]")
			os.Exit(1)
		}
		if *playlistList == true {
			fmt.Printf("Printing all current playlists for user: %v\n", user.ID)
			allUsersPlaylists, err := getCurrentPlaylists(ctx, client)
			if err != nil {
				fmt.Printf("unable to get user playlists: %v\n", err)
				os.Exit(1)
			}
			for _, v := range allUsersPlaylists.Playlists {
				fmt.Printf("name: %v\tid: %v\n", v.Name, v.ID)
			}
		}
		if *playlistPurgeFavTracks == true {
			fmt.Println("Purging tracks from the automated playlists")
			allUsersPlaylists, err := getCurrentPlaylists(ctx, client)
			if err != nil {
				fmt.Printf("unable to get user playlists: %v\n", err)
				os.Exit(1)
			}
			automatedPlaylists, err := getAutomatedPlaylists(ctx, client, user, allUsersPlaylists)
			if err != nil {
				fmt.Printf("getAutomatedPlaylists(ctx,client,%v,%v): %v", user, allUsersPlaylists, err)
				os.Exit(1)
			}
			for _, v := range automatedPlaylists {
				fmt.Printf("purging tracks on playlist %v\n", v.Name)
				err = purgeTracks(ctx, client, v)
				if err != nil {
					fmt.Printf("purgeTracks() failed: %v\n", err)
				}
			}
		}
		// TODO(dduclayan): Deal with duplicates
		// TODO(dduclayan): Refactor to google style guide
		if *playlistFill == true {
			allUsersPlaylists, err := getCurrentPlaylists(ctx, client)
			if err != nil {
				fmt.Printf("unable to get user playlists: %v", err)
				os.Exit(1)
			}
			automatedPlaylists, err := getAutomatedPlaylists(ctx, client, user, allUsersPlaylists)
			if err != nil {
				fmt.Printf("getAutomatedPlaylists(ctx,client,%v,%v): %v", user, allUsersPlaylists, err)
				os.Exit(1)
			}
			var shortTermConfig playlistConfig
			var medTermConfig playlistConfig
			var longTermConfig playlistConfig
			for _, v := range automatedPlaylists {
				if shortTermRe.MatchString(v.Name) {
					shortTermConfig = playlistConfig{
						name:          v.Name,
						public:        v.IsPublic,
						description:   v.Description,
						collaborative: v.Collaborative,
						duration:      spotify.ShortTermRange,
						user:          user,
						id:            v.ID,
					}
				}
				if medTermRe.MatchString(v.Name) {
					medTermConfig = playlistConfig{
						name:          v.Name,
						public:        v.IsPublic,
						description:   v.Description,
						collaborative: v.Collaborative,
						duration:      spotify.MediumTermRange,
						user:          user,
						id:            v.ID,
					}
				}
				if longTermRe.MatchString(v.Name) {
					longTermConfig = playlistConfig{
						name:          v.Name,
						public:        v.IsPublic,
						description:   v.Description,
						collaborative: v.Collaborative,
						duration:      spotify.LongTermRange,
						user:          user,
						id:            v.ID,
					}
				}
			}

			// TODO: Should errGroup here.
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				err := getTopTracksAndFill(ctx, &wg, client, shortTermConfig)
				if err != nil {
					fmt.Printf("getTopTracksAndFill() failed: %v", err)
					os.Exit(1)
				}
			}()
			go func() {
				err := getTopTracksAndFill(ctx, &wg, client, medTermConfig)
				if err != nil {
					fmt.Printf("getTopTracksAndFill() failed: %v", err)
					os.Exit(1)
				}
			}()
			go func() {
				err := getTopTracksAndFill(ctx, &wg, client, longTermConfig)
				if err != nil {
					fmt.Printf("getTopTracksAndFill() failed: %v", err)
					os.Exit(1)
				}
			}()
			wg.Wait()
		}
	}
	fmt.Printf("Done! Completed in %v\n", time2.Since(start).Truncate(time2.Millisecond))
}
