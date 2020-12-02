/*
Copyright 2020 Kamal Nasser All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/apex/log"
	"github.com/apex/log/handlers/cli"
	"github.com/digitalocean/godo"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var (
	doToken        = kingpin.Flag("access-token", "DigitalOcean API Token - if unset, attempts to use doctl's stored token of its current default context. env var: DIGITALOCEAN_ACCESS_TOKEN").Short('t').Envar("DIGITALOCEAN_ACCESS_TOKEN").String()
	sshUser        = kingpin.Flag("ssh-user", "default ssh user").String()
	sshPort        = kingpin.Flag("ssh-port", "default ssh port").Int()
	tag            = kingpin.Flag("tag", "filter droplets by tag").String()
	ignore         = kingpin.Flag("ignore", "ignore a Droplet by name, can be specified multiple times").Strings()
	groupByRegion  = kingpin.Flag("group-by-region", "group hosts by region, defaults to true").Default("true").Bool()
	groupByTag     = kingpin.Flag("group-by-tag", "group hosts by their Droplet tags, defaults to true").Default("true").Bool()
	groupByProject = kingpin.Flag("group-by-project", "group hosts by their Projects, defaults to true").Default("true").Bool()
	privateIPs     = kingpin.Flag("private-ips", "use private Droplet IPs instead of public IPs").Bool()
	out            = kingpin.Flag("out", "write the ansible inventory to this file - if unset, print to stdout").String()
	timeout        = kingpin.Flag("timeout", "timeout for total runtime of the command, defaults to 2m").Default("2m").Duration()
)

var doRegions = []string{"ams1", "ams2", "ams3", "blr1", "fra1", "lon1", "nyc1", "nyc2", "nyc3", "sfo1", "sfo2", "sfo3", "sgp1", "tor1"}

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

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := godo.NewFromToken(*doToken)

	// get droplets
	if *tag != "" {
		log.WithField("tag", *tag).Info("only selecting tagged Droplets")
	}

	log.Info("listing Droplets")
	droplets, err := listDroplets(ctx, client, *tag)
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
	dropletsByID := make(map[int]string, len(droplets))

	for _, d := range droplets {
		ll := log.WithField("droplet", d.Name)
		ll.Info("processing")

		dropletsByID[d.ID] = d.Name

		if *groupByRegion {
			r := d.Region.Slug
			dropletsByRegion[r] = append(dropletsByRegion[r], d.Name)
		}

		if *groupByTag {
			for _, tag := range d.Tags {
				dropletsByTag[tag] = append(dropletsByTag[tag], d.Name)
			}
		}

		var (
			ip  string
			err error
		)
		if *privateIPs {
			ip, err = d.PrivateIPv4()
		} else {
			ip, err = d.PublicIPv4()
		}
		if err != nil {
			ll.WithError(err).Error("couldn't look up the Droplet's IP address, skipped")
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
		if ip != "" {
			inventory.WriteString(fmt.Sprintf("ansible_host=%s", ip))
		} else {
			ll.Warn("could not get the Droplet's IP address, using hostname")
		}
		inventory.WriteRune('\n')
	}
	inventory.WriteRune('\n')

	// write the region groups
	if *groupByRegion {
		// loop over the doRegions slice to maintain alphabetic order
		for _, region := range doRegions {
			log.WithField("region", region).Info("building region group")
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
			tag = sanitizeAnsibleGroup(tag)
			log.WithField("tag", tag).Info("building tag group")

			inventory.WriteString(fmt.Sprintf("[%s]", tag))
			inventory.WriteRune('\n')

			for _, d := range droplets {
				inventory.WriteString(d)
				inventory.WriteRune('\n')
			}
			inventory.WriteRune('\n')
		}
	}

	// write the project groups
	if *groupByProject {
		log.Info("listing projects")
		projects, _, err := client.Projects.List(ctx, nil)
		if err != nil {
			log.WithError(err).Fatal("couldn't list projects")
		}

		dropletsByProject := make(map[string][]string)
		for _, project := range projects {
			ll := log.WithField("project", project.Name)
			ll.Info("listing project resources")

			resources, err := listProjectResources(ctx, client, project.ID)
			if err != nil {
				ll.WithError(err).Fatal("")
			}

			for _, r := range resources {
				if !strings.HasPrefix(r.URN, "do:droplet:") {
					continue
				}

				id := strings.TrimPrefix(r.URN, "do:droplet:")
				idInt, err := strconv.Atoi(id)
				if err != nil {
					ll.WithError(err).WithField("urn", r.URN).Error("parsing droplet ID, skipping")
					continue
				}

				// skip droplets that aren't included in the inventory
				droplet, exists := dropletsByID[idInt]
				if !exists {
					continue
				}

				dropletsByProject[project.Name] = append(dropletsByProject[project.Name], droplet)
			}
		}

		for project, droplets := range dropletsByProject {
			project = sanitizeAnsibleGroup(project)
			log.WithField("project", project).Info("building project group")

			inventory.WriteString(fmt.Sprintf("[%s]", project))
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

func sanitizeAnsibleGroup(s string) string {
	// replace invalid characters
	s = strings.NewReplacer(
		" ", "_",
		"-", "_",
		":", "_",
	).Replace(s)

	// group names cannot start with a digit
	if '0' <= s[0] && s[0] <= '9' {
		s = "_" + s
	}

	return s
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
func listDroplets(ctx context.Context, client *godo.Client, tag string) ([]godo.Droplet, error) {
	droplets := []godo.Droplet{}

	call := func(opt *godo.ListOptions) (interface{}, *godo.Response, error) {
		if tag != "" {
			return client.Droplets.ListByTag(ctx, tag, opt)
		}

		return client.Droplets.List(ctx, opt)
	}
	handler := func(d interface{}) error {
		dd, ok := d.([]godo.Droplet)
		if !ok {
			return fmt.Errorf("listing Droplets")
		}
		droplets = append(droplets, dd...)
		return nil
	}

	err := paginateGodo(ctx, call, handler)
	if err != nil {
		return nil, err
	}

	return droplets, nil
}

// get project resources w/ pagination
func listProjectResources(ctx context.Context, client *godo.Client, projectID string) ([]godo.ProjectResource, error) {
	prs := []godo.ProjectResource{}

	call := func(opt *godo.ListOptions) (interface{}, *godo.Response, error) {
		return client.Projects.ListResources(ctx, projectID, opt)
	}
	handler := func(r interface{}) error {
		rr, ok := r.([]godo.ProjectResource)
		if !ok {
			return fmt.Errorf("listing project resources")
		}
		prs = append(prs, rr...)
		return nil
	}

	err := paginateGodo(ctx, call, handler)
	if err != nil {
		return nil, err
	}

	return prs, nil
}

func paginateGodo(ctx context.Context, call func(*godo.ListOptions) (interface{}, *godo.Response, error), handler func(interface{}) error) error {
	// create options. initially, these will be blank
	opt := &godo.ListOptions{}
	for {
		results, resp, err := call(opt)
		if err != nil {
			return err
		}

		err = handler(results)
		if err != nil {
			return nil
		}

		// if we are at the last page, break out the for loop
		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}

		page, err := resp.Links.CurrentPage()
		if err != nil {
			return err
		}

		// set the page we want for the next request
		opt.Page = page + 1
	}

	return nil
}
