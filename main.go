package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"github.com/getlantern/systray/example/icon"
	"github.com/petoc/gbfs"
	"jonwillia.ms/biketray/bikeshare"
	"jonwillia.ms/biketray/geo"
	"jonwillia.ms/biketray/links"
	"jonwillia.ms/biketray/systems"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	systray.Run(func() { onReady(ctx) }, func() { onExit(ctx, cancel) })
}

var supportMultiline = runtime.GOOS != "darwin"

const timeFmt = time.RFC822

func onReady(ctx context.Context) {

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)
	defer signal.Stop(sigusr1)

	lat := flag.Float64("lat", math.NaN(), "lat")
	lon := flag.Float64("lon", math.NaN(), "lat")

	flag.Parse()
	_ = icon.Data
	systray.SetIcon(icon.Data) // 32x32
	systray.SetTitle("BikeTray")
	systray.SetTooltip("BikeTray")
	statusMenu := systray.AddMenuItem("Loading...", "")
	statusMenu.Disable()

	var locationF geo.LocationFunc = geo.Location

	if math.IsNaN(*lat) && math.IsNaN(*lon) {

	} else {
		locationF = func(ctx context.Context) (<-chan geo.LocationInfo, error) {

			// TODO allow reenabling real geo
			c := make(chan geo.LocationInfo, 1)
			wakeFakeGeo := make(chan geo.LocationInfo)
			go func() {
				g := geo.LocationInfo{Lat: *lat, Lon: *lon}
				for {
					fmt.Println("fake geo", g)
					c <- g
					select {
					case <-time.After(time.Minute):
					case g = <-wakeFakeGeo:
						fmt.Println("wake fake geo")
					}
				}
			}()
			mi := systray.AddMenuItem("Teleport to", "")
			go func() {
				for {
					for range sigusr1 {
						mi.Show()
					}
				}
			}()

			var teleportItems []*systray.MenuItem
			teleportLocs := []geo.LocationInfo{
				{"Central Park", 40.785091, -73.968285, nil},
				{"Spanish Steps, Rome", 41.905991, 12.482775, nil},
				{"Corona Heights Park, SF", 37.765678, -122.438713, nil},
				{"Montreal", 45.508888, -73.561668, nil},
				{"Buckingham Palace", 51.501476, -0.140634, nil},
				{"Soldier Field, Chicago", 41.862366, -87.617256, nil},
				{Description: "Omphalos"},
			}

			for _, geo := range teleportLocs {
				geo := geo
				si := mi.AddSubMenuItemCheckbox(geo.Description, "", *lat == geo.Lat && *lon == geo.Lon)
				teleportItems = append(teleportItems, si)
				go func() {
					for {
						<-si.ClickedCh
						fmt.Println("teleport to ", geo.Description, geo)
						wakeFakeGeo <- geo
						for _, ti := range teleportItems {
							if ti != si {
								ti.Uncheck()
							}
						}
					}
				}()
			}
			systray.AddSeparator()
			return c, nil
		}
	}

	locationF = geo.RateLimit(locationF, 5, 15*time.Second)

	locChan, err := locationF(ctx)
	if err != nil {
		log.Fatalf("locationF: %v", err)
	}

	geoMgr := geo.NewManager(ctx, locChan)

	MenuItemLocation(ctx, geoMgr)

	menusForSystem := make(map[systems.System]*systray.MenuItem)
	subMenus := make(map[*systray.MenuItem][]*systray.MenuItem)

	const maxSystems = 20

	pool := make(map[*systray.MenuItem]struct{}, maxSystems)
	get := func() *systray.MenuItem {
		for mi, _ := range pool {
			return mi
		}
		panic("no more top level menus")
	}
	put := func(mi *systray.MenuItem) {
		pool[mi] = struct{}{}
	}

	for i := 0; i < maxSystems; i++ {
		mi := systray.AddMenuItem("uninitialized system", "")
		mi.Hide()
		put(mi)
	}

	// TODO no mutex
	var clickHandlers map[*systray.MenuItem]func() = make(map[*systray.MenuItem]func())

	initSubMenus := func(mi *systray.MenuItem, system systems.System) {
		for i := 0; i < 10; i++ {
			sub := mi.AddSubMenuItem("", "")
			sub.Hide()
			go func() {
				for {
					<-sub.ClickedCh
					ch, ok := clickHandlers[sub]
					if !ok {
						continue
					}
					ch()
				}
			}()
			subMenus[mi] = append(subMenus[mi], sub)
		}
	}

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the whole app")
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	start := time.Now()
	csvSystems := systems.Load() // slow!

	statusMenu.SetTitle(fmt.Sprintf("Loading %d systems", len(csvSystems)))

	clientsC := make(chan map[systems.System]*gbfs.Client, 1)
	go func() {
		clients := systems.Test(csvSystems) // slow!
		dur := time.Since(start)
		log.Println("boot duration", len(clients), dur)
		systems.StopRecorder()
		statusMenu.SetTitle(fmt.Sprintf("Loading %d active systems", len(clients)))
		select {
		case <-ctx.Done():
			return
		case clientsC <- clients:
		}
	}()

	systemsNearbyC := systems.Nearby(ctx, clientsC, geoMgr)
	bsMgr := bikeshare.NewManager(ctx, geoMgr, systemsNearbyC)

	type activeSystem struct {
		NearbyResult systems.NearbyResult
		Cancel       func()
	}

	for {
		select {
		case nrs := <-bsMgr.NearbyResults():
			statusMenu.SetTitle(fmt.Sprintf("Loading %d nearby systems", len(nrs)))
			log.Println("visible systems update", len(nrs))
			for system, mi := range menusForSystem {
				if nr, ok := nrs[system]; ok {
					mCiti, ok := menusForSystem[nr.System]
					if !ok {
						log.Println("get")
						mCiti = get()
						menusForSystem[nr.System] = mCiti
					}
					name := fmt.Sprintf("%s (%s)", nr.System.Name, nr.System.Location)
					mCiti.SetTitle(name)
					mCiti.Show()
					continue
				}
				delete(menusForSystem, system)
				mi.Hide()
				put(mi)
			}
		case cr := <-bsMgr.ClientResults():
			statusMenu.Hide()
			systray.SetTitle("")
			mCiti, ok := menusForSystem[cr.System]
			if !ok {
				log.Println("get")
				mCiti = get()
				menusForSystem[cr.System] = mCiti
			}
			name := fmt.Sprintf("%s (%s)", cr.System.Name, cr.System.Location)
			mCiti.SetTitle(name)
			mCiti.Show()
			mStations, ok := subMenus[mCiti]
			if !ok {
				initSubMenus(mCiti, cr.System)
				mStations, _ = subMenus[mCiti]
			}

			mCiti.SetTooltip(time.Now().Format(timeFmt))
			for i, mi := range mStations {
				if i >= len(cr.Data) {
					mi.Hide()
					continue
				}
				//mi.Check()
				mi.Show()
				title := cr.Data[i].Label
				sparkline := cr.Data[i].Sparkline
				if supportMultiline {
					title += sparkline
				} else {
					mi.SetTooltip(sparkline)
				}
				mi.SetTitle(title)

				clickHandlers[mi] = func() {
					cl, ok := geoMgr.CurrentLocation()
					l := &cl
					if !ok {
						l = nil
					}
					err := links.OpenLocation(l, cr.Data[i].LocationInfo)
					if err != nil {
						log.Println("click handler", err)
					}
				}
			}
		}
	}
}

func onExit(ctx context.Context, cancel func()) {
	defer cancel()
}
