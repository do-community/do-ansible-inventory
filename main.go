package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/apex/log"
	"github.com/apex/log/handlers/cli"
	"github.com/digitalocean/godo"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var (
	doToken       = kingpin.Flag("access-token", "DigitalOcean API Token - if unset, attempts to use doctl's stored token of its current default context. env var: DIGITALOCEAN_ACCESS_TOKEN").Short('t').Envar("DIGITALOCEAN_ACCESS_TOKEN").String()
	sshUser       = kingpin.Flag("ssh-user", "default ssh user").String()
	sshPort       = kingpin.Flag("ssh-port", "default ssh port").Int()
	tag           = kingpin.Flag("tag", "filter droplets by tag").String()
	ignore        = kingpin.Flag("ignore", "ignore a Droplet by name, can be specified multiple times").Strings()
	groupByRegion = kingpin.Flag("group-by-region", "group hosts by region, defaults to true").Default("true").Bool()
	groupByTag    = kingpin.Flag("group-by-tag", "group hosts by their Droplet tags, defaults to true").Default("true").Bool()
	out           = kingpin.Flag("out", "write the ansible inventory to this file").String()
)

var doRegions = []string{"ams1", "ams2", "ams3", "blr1", "fra1", "lon1", "nyc1", "nyc2", "nyc3", "sfo1", "sfo2", "sfo3", "sgp1", "tor1"}

func doctlToken() (string, string, error) {
	type doctlConfig struct {
		Context      string            `yaml:"context"`
		AccessToken  string            `yaml:"access-token"`
		AuthContexts map[string]string `yaml:"auth-contexts"`
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("couldn't look up user config dir: %w", err)
	}

	cfgFile, err := ioutil.ReadFile(filepath.Join(cfgDir, "doctl", "config.yaml"))
	if err != nil {
		return "", "", fmt.Errorf("couldn't read doctl's config.yaml: %w", err)
	}

	cfg := doctlConfig{}
	err = yaml.Unmarshal(cfgFile, &cfg)
	if err != nil {
		return "", "", fmt.Errorf("couldn't unmarshal doctl's config.yaml: %w", err)
	}

	switch cfg.Context {
	case "default":
		return cfg.AccessToken, cfg.Context, nil
	default:
		return cfg.AuthContexts[cfg.Context], cfg.Context, nil
	}
}

func main() {
	kingpin.Parse()
	log.SetHandler(cli.Default)

	if *doToken == "" {
		log.Info("no access token provided, attempting to look up doctl's access token")
		token, context, err := doctlToken()
		if err != nil {
			log.WithError(err).Fatalf("couldn't look up token")
		}

		*doToken = token
		log.WithField("context", context).Info("using doctl access token")
	}

	ctx := context.Background()
	client := godo.NewFromToken(*doToken)

	// get droplets
	if *tag != "" {
		log.WithField("tag", *tag).Info("only selecting tagged Droplets")
	}

	droplets, err := dropletList(ctx, client, *tag)
	if err != nil {
		log.WithError(err).Fatal("couldn't fetch Droplets")
	}

	// filter out ignored droplets
	droplets = removeIgnored(droplets, *ignore)

	// initialize some maps
	var dropletsByRegion map[string][]string
	if *groupByRegion {
		dropletsByRegion = make(map[string][]string, len(doRegions))
		for _, r := range doRegions {
			dropletsByRegion[r] = []string{}
		}
	}

	var dropletsByTag map[string][]string
	if *groupByTag {
		dropletsByTag = make(map[string][]string, 0)
	}

	var inventory bytes.Buffer

	for _, d := range droplets {
		log.WithField("droplet", d.Name).Info("processing")
		if *groupByRegion {
			r := d.Region.Slug
			dropletsByRegion[r] = append(dropletsByRegion[r], d.Name)
		}

		if *groupByTag {
			for _, tag := range d.Tags {
				dropletsByTag[tag] = append(dropletsByTag[tag], d.Name)
			}
		}

		ip, err := d.PublicIPv4()
		if err != nil {
			log.WithError(err).WithField("droplet", d.Name).Error("couldn't look up the Droplet's IP address, skipped")
			continue
		}

		inventory.WriteString(d.Name)
		inventory.WriteRune('\t')
		if *sshUser != "" {
			inventory.WriteString(fmt.Sprintf("ansible_user=%s ", *sshUser))
		}
		if *sshPort != 0 {
			inventory.WriteString(fmt.Sprintf("ansible_port=%d ", *sshPort))
		}
		inventory.WriteString(fmt.Sprintf("ansible_host=%s", ip))
		inventory.WriteRune('\n')
	}
	inventory.WriteRune('\n')

	// write the region groups
	if *groupByRegion {
		// loop over the doRegions slice to maintain alphabetic order
		for _, region := range doRegions {
			droplets := dropletsByRegion[region]

			inventory.WriteString(fmt.Sprintf("[%s]", region))
			inventory.WriteRune('\n')

			for _, d := range droplets {
				inventory.WriteString(d)
				inventory.WriteRune('\n')
			}
			inventory.WriteRune('\n')
		}
	}

	// write the tag groups
	if *groupByTag {
		for tag, droplets := range dropletsByTag {
			inventory.WriteString(fmt.Sprintf("[%s]", tag))
			inventory.WriteRune('\n')

			for _, d := range droplets {
				inventory.WriteString(d)
				inventory.WriteRune('\n')
			}
			inventory.WriteRune('\n')
		}
	}

	if *out != "" {
		ll := log.WithField("out", *out)
		ll.Info("writing inventory to file")
		f, err := os.Create(*out)
		if err != nil {
			ll.WithError(err).Fatal("couldn't open file for writing")
		}
		defer f.Close()

		_, err = inventory.WriteTo(f)
		if err != nil {
			ll.WithError(err).Fatal("couldn't write inventory to file")
		}
	} else {
		inventory.WriteTo(os.Stdout)
	}

	log.Info("done!")
}

func removeIgnored(droplets []godo.Droplet, ignored []string) []godo.Droplet {
	if len(ignored) == 0 {
		return droplets
	}

	// copy ignored droplets into a map
	ignoreList := make(map[string]interface{}, len(ignored))
	for _, i := range ignored {
		ignoreList[i] = struct{}{}
	}

	// remove ignored droplets from the list
	newDroplets := droplets[:0]
	for _, d := range droplets {
		if _, ignored := ignoreList[d.Name]; ignored {
			log.WithField("droplet", d.Name).Info("ignoring")
			continue
		}

		newDroplets = append(newDroplets, d)
	}

	return newDroplets
}

// get droplets w/ pagination
func dropletList(ctx context.Context, client *godo.Client, tag string) ([]godo.Droplet, error) {
	// create a list to hold our droplets
	list := []godo.Droplet{}

	// create options. initially, these will be blank
	opt := &godo.ListOptions{}
	for {
		var (
			droplets []godo.Droplet
			resp     *godo.Response
			err      error
		)
		if tag != "" {
			droplets, resp, err = client.Droplets.ListByTag(ctx, tag, opt)
		} else {
			droplets, resp, err = client.Droplets.List(ctx, opt)
		}

		if err != nil {
			return nil, err
		}

		// append the current page's droplets to our list
		for _, d := range droplets {
			list = append(list, d)
		}

		// if we are at the last page, break out the for loop
		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}

		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}

		// set the page we want for the next request
		opt.Page = page + 1
	}

	return list, nil
}
