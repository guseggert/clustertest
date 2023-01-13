package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	httpapi "github.com/ipfs/go-ipfs-http-client"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"golang.org/x/sync/errgroup"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	f, err := os.Open("/home/gus/Downloads/peers+maddrs.csv")
	must(err)
	peers := map[peer.ID][]multiaddr.Multiaddr{}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		split := strings.SplitN(line, ",", 2)
		peerIDStr := split[0]
		addrs := split[1]

		peerID, err := peer.Decode(peerIDStr)
		must(err)

		addrsSplit := strings.Split(addrs, ",")
		for _, addr := range addrsSplit {
			addr = strings.ReplaceAll(addr, "{", "")
			addr = strings.ReplaceAll(addr, `"`, "")
			addr = strings.ReplaceAll(addr, `}`, "")
			ma, err := multiaddr.NewMultiaddr(addr)
			must(err)
			peers[peerID] = append(peers[peerID], ma)
		}
	}

	group, ctx := errgroup.WithContext(context.Background())
	group.SetLimit(200)
	httpClient := http.DefaultClient
	for peerID, addrs := range peers {
		for _, addr := range addrs {
			addr := addr
			multiaddr.ForEach(addr, func(c multiaddr.Component) bool {
				if c.Protocol().Code == multiaddr.P_IP4 {
					tcpComp, err := multiaddr.NewComponent("tcp", "5001")
					must(err)
					ma := c.Encapsulate(tcpComp)

					group.Go(func() error {
						http.Get("http://")
						client, err := httpapi.NewApiWithClient(ma, httpClient)
						must(err)

						fmt.Printf("%s\n", ma)
						f, err := os.Create("/home/gus/prom/" + peerID.String())
						if err != nil {
							fmt.Printf("%s\n", err)
							return nil
						}
						defer f.Close()

						pins, err := client.Pin().Ls(ctx)
						if err != nil {
							fmt.Printf("%s\n", err)
							return nil
						}
						for pin := range pins {
							cid := pin.Path().Cid()
							f.WriteString(cid.String() + "\n")
						}

						// rqb := client.Request("config/show")
						// resp, err := rqb.Send(ctx)
						// if err != nil {
						// 	fmt.Printf("%s\n", err)
						// 	return nil
						// }
						// if resp.Output == nil {
						// 	return nil
						// }
						// fmt.Printf("%s\n", ma)
						// f, err := os.Create("/home/gus/files/" + peerID.String())
						// if err != nil {
						// 	fmt.Printf("%s\n", err)
						// 	return nil
						// }
						// defer f.Close()
						// _, err = io.Copy(f, resp.Output)
						// require.NoError(t, err)

						// b, err := io.ReadAll(resp.Output)
						// require.NoError(t, err)
						// fmt.Printf("result: %s\n", string(b))

						return nil
					})
					return false
				}
				return true
			})

		}
	}
	group.Wait()

}
