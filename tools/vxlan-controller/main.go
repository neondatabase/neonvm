package main

import (
	"context"
	"flag"
	"net"
	"os"
	"syscall"
	"time"
	//	"encoding/base64"
	//	"encoding/json"

	//	"bytes"
	//	"flag"
	//	"fmt"
	//	"io/ioutil"
	"log"
	//	"math"
	//	"net"
	//	"os"
	//	"os/exec"
	//	"path/filepath"
	//	"regexp"
	//	"strings"
	//	"sync"

	//	"github.com/alessio/shellescape"
	//	"github.com/cilium/cilium/pkg/mac"
	//	"github.com/docker/docker/pkg/ioutils"
	//	"github.com/docker/libnetwork/types"
	//	"github.com/kdomanski/iso9660"
	//	"github.com/vishvananda/netlink"
	//	"k8s.io/apimachinery/pkg/api/resource"

	//	vmv1 "github.com/neondatabase/neonvm/apis/neonvm/v1"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	VXLAN_IF_NAME     = "neon-vxlan0"
	VXLAN_BRIDGE_NAME = "neon-br0"
	VXLAN_ID          = 100

	overlayCidrVariableName = "OVERLAY_CIDR"
)

var (
	delete = flag.Bool("delete", false, `delete VXLAN interfaces`)
)

func main() {
	flag.Parse()

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	// -delete option used for teardown vxlan setup
	if *delete {
		// delete routes
		overlayNet := os.Getenv(overlayCidrVariableName)
		if len(overlayNet) != 0 {
			log.Printf("deleting route to overlay network '%s'", overlayNet)
			if err := routeDel(overlayNet, VXLAN_BRIDGE_NAME); err != nil {
				log.Print(err)
			}
		}
		log.Printf("deleting vxlan interface %s", VXLAN_IF_NAME)
		if err := deleteLink(VXLAN_IF_NAME); err != nil {
			log.Print(err)
		}
		log.Printf("deleting bridge interface %s", VXLAN_BRIDGE_NAME)
		if err := deleteLink(VXLAN_BRIDGE_NAME); err != nil {
			log.Print(err)
		}
		os.Exit(0)
	}

	ownNodeIP := os.Getenv("MY_NODE_IP")
	log.Printf("own node IP: %s", ownNodeIP)

	// create linux bridge
	log.Printf("creating linux bridge interface (name: %s)", VXLAN_BRIDGE_NAME)
	if err := createBrigeInterface(VXLAN_BRIDGE_NAME); err != nil {
		log.Fatal(err)
	}

	// create vxlan
	log.Printf("creating vxlan interface (name: %s, id: %d)", VXLAN_IF_NAME, VXLAN_ID)
	if err := createVxlanInterface(VXLAN_IF_NAME, VXLAN_ID, ownNodeIP, VXLAN_BRIDGE_NAME); err != nil {
		log.Fatal(err)
	}

	for {
		log.Print("getting nodes IP addresses")
		nodeIPs, err := getNodesIPs(clientset)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("found %d ip addresses", len(nodeIPs))

		// update FDB
		log.Print("udpate FDB table")
		if err := updateFDB(VXLAN_IF_NAME, nodeIPs, ownNodeIP); err != nil {
			log.Fatal(err)
		}

		// update routes
		overlayNet := os.Getenv(overlayCidrVariableName)
		if len(overlayNet) != 0 {
			log.Printf("upsert route to overlay network '%s' via '%s'", overlayNet, VXLAN_BRIDGE_NAME)
			if err := routeAdd(overlayNet, VXLAN_BRIDGE_NAME); err != nil {
				log.Fatal(err)
			}
		}

		time.Sleep(30 * time.Second)
	}
}

func getNodesIPs(clientset *kubernetes.Clientset) ([]string, error) {
	ips := []string{}
	// exclude control-plane nodes from list
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane!=",
	})
	if err != nil {
		return ips, err
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ips = append(ips, a.Address)
			}
		}
	}
	return ips, nil
}

func createBrigeInterface(name string) error {
	// check if interface already exists
	_, err := netlink.LinkByName(name)
	if err == nil {
		log.Printf("link with name %s already found", name)
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}

	// create an configure linux bridge
	link := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	}
	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func createVxlanInterface(name string, vxlanID int, ownIP string, bridgeName string) error {
	// check if interface already exists
	_, err := netlink.LinkByName(name)
	if err == nil {
		log.Printf("link with name %s already found", name)
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}

	// create an configure vxlan
	link := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
		VxlanId: vxlanID,
		SrcAddr: net.ParseIP(ownIP),
		Port:    4789,
	}

	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	// add vxlan to bridge
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMaster(link, br); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func updateFDB(vxlanName string, nodeIPs []string, ownIP string) error {

	broadcastFdbMac, _ := net.ParseMAC("00:00:00:00:00:00")

	// get vxlan interface details
	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return err
	}

	for _, ip := range nodeIPs {
		if ip != ownIP {
			broadcastFdbEntry := netlink.Neigh{
				LinkIndex:    link.Attrs().Index,
				Family:       syscall.AF_BRIDGE,
				State:        netlink.NUD_PERMANENT,
				Flags:        netlink.NTF_SELF,
				IP:           net.ParseIP(ip),
				HardwareAddr: broadcastFdbMac,
			}
			// add entry to FDB table
			// duplicate append action will not case error.
			log.Printf("add/update FDB broadcast entry via %s", ip)
			if err := netlink.NeighAppend(&broadcastFdbEntry); err != nil {
				return err
			}
		}
	}

	return nil
}

func routeAdd(cidr string, iface string) error {
	// get interface details
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	// parse CIDR the retrive network
	_, ipv4Net, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	// add/replace route
	route := netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       ipv4Net,
	}
	if netlink.RouteReplace(&route); err != nil {
		return err
	}

	return nil
}

func routeDel(cidr string, iface string) error {
	// get interface details
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	// parse CIDR the retrive network
	_, ipv4Net, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	// del route
	route := netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       ipv4Net,
	}
	if netlink.RouteDel(&route); err != nil {
		return err
	}

	return nil
}

func deleteLink(name string) error {
	// check if interface already exists
	link, err := netlink.LinkByName(name)
	if err == nil {
		if err := netlink.LinkDel(link); err != nil {
			return err
		}
		log.Printf("link with name %s was deleted", name)
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}
	log.Printf("link with name %s not found", name)

	return nil
}
