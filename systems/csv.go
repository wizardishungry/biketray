package systems

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Eraac/gbfs"
	gbfsspec "github.com/Eraac/gbfs/spec/v2.0"
	"github.com/StefanSchroeder/Golang-Ellipsoid/ellipsoid"
	"github.com/dnaeon/go-vcr/v2/recorder"
	"golang.org/x/sync/errgroup"
	"jonwillia.ms/biketray/geo"
)

const CSV = "https://raw.githubusercontent.com/NABSA/gbfs/master/systems.csv"
const systemDist = 60000 // meters

type System struct {
	CountryCode      string
	Name             string
	Location         string
	SystemID         string
	URL              string
	AutoDiscoveryURL string
}

func Load() []System {
	r, err := http.NewRequest(http.MethodGet, CSV, nil)
	if err != nil {
		panic(err)
	}
	resp, err := httpClient.Do(r)
	if err != nil {
		panic(err)
	}
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	reader := csv.NewReader(resp.Body)
	records, err := reader.ReadAll()
	if err != nil {
		panic(err)
	}
	if len(records) < 2 {
		panic("len records wrong")
	}
	if len(records[0]) != 6 {
		panic("num calls wrong")
	}

	systems := make([]System, 0)

	for _, systemRow := range records[1:] {
		system := System{
			CountryCode:      systemRow[0],
			Name:             systemRow[1],
			Location:         systemRow[2],
			SystemID:         systemRow[3],
			URL:              systemRow[4],
			AutoDiscoveryURL: systemRow[5],
		}
		systems = append(systems, system)
	}
	for _, system := range systems {
		fmt.Println(system.AutoDiscoveryURL)
	}
	return systems
}

func Test(systems []System) map[System]gbfs.Client {

	// system := systems["bird-new-york"]

	ctx := context.TODO()
	g, ctx := errgroup.WithContext(ctx)

	var (
		mutex         sync.Mutex
		systemClients map[System]gbfs.Client = make(map[System]gbfs.Client)
	)
	g.SetLimit(16)
	for _, system := range systems {
		system := system
		g.Go(func() error {
			// if system.AutoDiscoveryURL != "https://data.lime.bike/api/partners/v2/gbfs/new_york/gbfs.json" {
			// 	return nil
			// }
			c := getSystemInfo(system)

			if c != nil {
				mutex.Lock()
				systemClients[system] = c
				mutex.Unlock()
			} else {
				log.Println("can't load", system.AutoDiscoveryURL)
			}
			return nil
		})
	}

	err := g.Wait()
	if err != nil {
		panic(err)
	}

	fmt.Println("Test done", len(systems), len(systemClients))
	return systemClients
}

var httpClient, StopRecorder = func() (http.Client, func() error) {
	r, err := recorder.New("http-cache")
	if err != nil {
		log.Fatalf("recorder.New: %v", err)
	}
	r.SkipRequestLatency = true

	r.Passthroughs = append(r.Passthroughs,
		func(req *http.Request) bool {
			return !strings.HasSuffix(req.URL.Path, "/system_information.json")
		},
	)

	return http.Client{
		Timeout:   10 * time.Second,
		Transport: r,
	}, r.Stop
}()

type AutoDiscovery struct {
	Data struct {
		En struct {
			Feeds []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"feeds"`
		} `json:"en"`
	} `json:"data"`
	LastUpdated int64 `json:"last_updated"`
	TTL         int64 `json:"ttl"`
}

func tryAuto(system System) []gbfs.HTTPOption {

	req, err := http.NewRequest(http.MethodGet, system.AutoDiscoveryURL, nil)
	if err != nil {
		log.Println("tryauto NewRequest", system.AutoDiscoveryURL, err)
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Println("tryauto", system.AutoDiscoveryURL, err)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		log.Println("tryauto", system.AutoDiscoveryURL, resp.Status)
		return nil

	}
	d := json.NewDecoder(resp.Body)
	var a AutoDiscovery
	err = d.Decode(&a)
	if err != nil {
		log.Println("tryauto json", system.AutoDiscoveryURL, err)
		return nil

	}
	_ = a
	fmt.Println("ok")
	if len(a.Data.En.Feeds) > 0 {
		var opts = []gbfs.HTTPOption{
			gbfs.HTTPOptionBaseURL(system.SystemID),
		}
		for _, f := range a.Data.En.Feeds {
			opts = append(opts, gbfs.HTTPOptionForceURL(f.Name, f.URL))
		}
		return opts
	}

	return nil
}

func getSystemInfo(system System) gbfs.Client {
	autoOpts := tryAuto(system)

	baseOpts := []gbfs.HTTPOption{
		gbfs.HTTPOptionClient(httpClient),
		gbfs.HTTPOptionBaseURL(system.AutoDiscoveryURL),
	}

	multiOpts := [][]gbfs.HTTPOption{
		append([]gbfs.HTTPOption{}, baseOpts...),
		append([]gbfs.HTTPOption{gbfs.HTTPOptionLanguage("en")}, baseOpts...),
	}

	if autoOpts != nil {
		autoOpts = append(autoOpts, baseOpts...)
		multiOpts = append(multiOpts, autoOpts)
	}

	if strings.HasSuffix(system.AutoDiscoveryURL, "gbfs.json") {

		u, _ := url.Parse(system.AutoDiscoveryURL)
		u.Path = path.Dir(u.Path)
		newURL := u.String()
		var newMultiOpts [][]gbfs.HTTPOption
		for _, opts := range multiOpts {
			newMultiOpts = append(newMultiOpts, opts)
			var newOpts = append([]gbfs.HTTPOption{}, opts...)
			newOpts = append(newOpts, gbfs.HTTPOptionBaseURL(newURL))
			newMultiOpts = append(newMultiOpts, newOpts)
		}
		multiOpts = newMultiOpts
	}

	var mutex sync.Mutex
	var client gbfs.Client

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(len(multiOpts) - 1)
	for _, opts := range multiOpts {
		opts := opts
		//	g.Go(func() error {
		c := getSystemInfoWithOpts(system, opts...)
		if c != nil {
			cancel()
			mutex.Lock()
			client = c
			mutex.Unlock()
		}
		// return nil
		//	})

	}

	return client
}
func getSystemInfoWithOpts(system System, opts ...gbfs.HTTPOption) gbfs.Client {

	c, err := gbfs.NewHTTPClient(opts...)
	if err != nil {
		panic(err)
	}

	var si gbfsspec.FeedSystemInformation
	if err := c.Get(gbfsspec.FeedKeySystemInformation, &si); err != nil {
		fmt.Println(system.Name, "err", err, system.AutoDiscoveryURL)
		return nil
	}

	fmt.Println(si.Data.Name)
	return c
}

type NearbyResult struct {
	System                 System
	FeedStationInformation gbfsspec.FeedStationInformation
	// StationInformation     gbfsspec.StationInformation
	Client gbfs.Client
}

func Nearby(ctx context.Context, clients map[System]gbfs.Client, mgr *geo.Manager) <-chan map[System]NearbyResult {
	c := make(chan map[System]NearbyResult, 1)
	geo1 := ellipsoid.Init("WGS84", ellipsoid.Degrees, ellipsoid.Meter, ellipsoid.LongitudeIsSymmetric, ellipsoid.BearingIsSymmetric)

	go func() {

		locC := mgr.Subscribe()
		defer mgr.Unsubscribe(locC)
		location := <-locC

		g, _ := errgroup.WithContext(ctx)
		var (
			mutex       sync.Mutex
			initResults map[System]NearbyResult = make(map[System]NearbyResult)
		)

		for system, client := range clients {
			system, client := system, client
			g.Go(func() error {
				var si gbfsspec.FeedStationInformation

				if err := client.Get(gbfsspec.FeedKeyStationInformation, &si); err != nil {
					fmt.Println("station info", err)
					return nil
				}

				mutex.Lock()
				initResults[system] = NearbyResult{
					System:                 system,
					FeedStationInformation: si,
					// StationInformation:     station,
					Client: client,
				}
				mutex.Unlock()

				return nil
			})
		}

		if err := g.Wait(); err != nil {
			panic(err)
		}

		for ctx.Err() == nil {

			dist := func(s gbfsspec.StationInformation) float64 {
				distance, _ := geo1.To(location.Lat, location.Lon, s.Latitude, s.Longitude)
				return distance
			}

			var results map[System]NearbyResult = make(map[System]NearbyResult)

		NEXT_SYSTEM:
			for k, v := range initResults {
				for _, station := range v.FeedStationInformation.Data.Stations {
					d := dist(station)
					if d < systemDist {
						fmt.Println("station in range", v.System.Name, station.Name, d)
						results[k] = v
						continue NEXT_SYSTEM
					}
				}
			}

			fmt.Println("Nearby", len(results))
			c <- results
			select {
			case <-ctx.Done():
				return
			case location = <-locC:
			}

		}
	}()
	return c
}
