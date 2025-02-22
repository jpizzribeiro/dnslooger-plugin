package dnslogger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/allegro/bigcache/v3"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/request"
	"github.com/elliotwutingfeng/go-fasttld"
	"github.com/maypok86/otter"
	"strconv"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"

	"github.com/miekg/dns"
)

// Define log to be a logger with the plugin name in it. This way we can just use log.Info and
// friends to log.
var log = clog.NewWithPlugin("dnslogger")
var extractor, _ = fasttld.New(fasttld.SuffixListParams{})

// DNSLogger is an example plugin to show how to write a plugin.
type DNSLogger struct {
	Next       plugin.Handler
	Categories map[int]Category
	Sources    map[string]SourceConfig
	BlockIp    string
	DuckDbPath string
	SocketAddr string
	Client     *UDPClient
	DB         *sql.DB
	Cache      otter.CacheWithVariableTTL[string, *DuckRow]
	BigCache   *bigcache.BigCache
}

type SourceConfig struct {
	BlockCategories     map[int]struct{}
	WhitelistCategories map[int]struct{}
}

type LogEntry struct {
	DateTime         string `json:"datetime"`
	Domain           string `json:"domain"`
	RegisteredDomain string `json:"registered_domain"`
	CategoryId       int    `json:"category"`
	SourceIp         string `json:"source_ip"`
	Type             string `json:"type"`
	AccessType       string `json:"access_type"`
}

type Category struct {
	ID   int
	Name string
}

type DuckRow struct {
	Domain     string
	CategoryId int
	Categories string
}

func (dl DNSLogger) searchDomainOnBigCache(domain string) *DuckRow {
	log.Infof("Searching domain on BigCache: %s", domain)
	row := &DuckRow{}

	if strings.HasSuffix(domain, ".") {
		domain = strings.TrimSuffix(domain, ".")
	}

	domainParts := generateDomainParts(domain)

	i := 0
	for _, domainPart := range domainParts {
		entry, err := dl.BigCache.Get(domainPart)
		if err == nil {
			row.Domain = domainPart
			row.CategoryId = 9999
			row.Categories = string(entry)
			i += 1
			break
		}
	}

	if i == 0 {
		return nil
	}

	return row
}

func (dl DNSLogger) searchDomainOnDuck(name string) *DuckRow {
	log.Info("Searching domain on Duck")
	row := &DuckRow{}

	if strings.HasSuffix(name, ".") {
		name = strings.TrimSuffix(name, ".")
	}

	domainParts := generateDomainParts(name)

	err := dl.DB.QueryRow(fmt.Sprintf(`SELECT domain, category_id
		FROM domains
		WHERE domain IN ('%s')
		ORDER BY LENGTH(domain) DESC
		LIMIT 1;`, strings.Join(domainParts, "','"))).Scan(&row.Domain, &row.CategoryId)
	if err != nil {
		log.Infof("%s not found on Duck", name)
		log.Error(err)
		return nil
	}

	return row
}

func (dl DNSLogger) emitToUDPSocket(logEntry LogEntry) {
	msg, err := json.Marshal(logEntry)
	if err == nil {
		if dl.Client != nil {
			if err := dl.Client.Send(string(msg) + "\n"); err != nil {
				clog.Warningf("Error sending log via UDP: %v", err)
			}
		}
	}
}

func generateDomainPartsReverse(domain string) []string {
	parts := strings.Split(domain, ".")
	var domains []string

	// Gerar os subdomínios do maior para o menor
	for i := len(parts) - 1; i >= 0; i-- {
		domains = append(domains, strings.Join(parts[i:], "."))
	}

	return domains
}

func generateDomainParts(domain string) []string {
	parts := strings.Split(domain, ".")
	var domains []string

	// Gerar os subdomínios relevantes
	for i := 0; i < len(parts)-1; i++ {
		domains = append(domains, strings.Join(parts[i:], "."))
	}

	return domains
}

// ServeDNS implements the plugin.Handler interface. This method gets called when example is used
// in a Server.
func (dl DNSLogger) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	// Captura o estado da requisição
	state := request.Request{W: w, Req: r}
	ip := state.IP()
	name := state.Name()
	qType := dns.TypeToString[state.QType()]

	logEntryJson := LogEntry{
		DateTime:   time.Now().Format(time.RFC3339),
		Domain:     name,
		SourceIp:   ip,
		Type:       qType,
		AccessType: "PASS",
	}

	var cacheKey = fmt.Sprintf("%s_%s", ip, name)

	var row *DuckRow
	onCache := false

	tld, err := extractor.Extract(fasttld.URLParams{URL: name})
	if err == nil {
		log.Infof("Domain: %s", tld.Domain)
		log.Infof("RegisteredDomain: %s", tld.RegisteredDomain)
		log.Infof("SubDomain: %s", tld.SubDomain)

		rowOnCache, ok := dl.Cache.Get(cacheKey)
		if ok {
			onCache = true
			row = rowOnCache

			log.Infof("get duckrow on cache")
		}

		if !onCache {
			/*start := time.Now()
			row = dl.searchDomainOnDuck(strings.TrimSpace(name))
			if row != nil {
				log.Infof("Domain: %s", row.Domain)
				log.Infof("CategoryId: %d", row.CategoryId)
				log.Infof("Category: %s", dl.Categories[row.CategoryId].Name)
			}
			duration := time.Since(start)
			fmt.Printf("Execução no DuckDB levou %s\n", duration)*/

			start2 := time.Now()
			row = dl.searchDomainOnBigCache(strings.TrimSpace(state.Name()))
			if row != nil {
				log.Infof("Domain: %s", row.Domain)
				log.Infof("CategoryId: %d", row.CategoryId)
				log.Infof("Category: %s", row.Categories)
			}
			duration2 := time.Since(start2)
			fmt.Printf("Execução no BigCache levou %s\n", duration2)

		}

		logEntryJson.RegisteredDomain = tld.RegisteredDomain

		var categories []string
		sourceIp, exists := dl.Sources[ip]
		if exists {
			if row != nil {
				if row.CategoryId == 9999 {
					categories = strings.Split(row.Categories, ",")
					for _, category := range categories {
						cat, _ := strconv.Atoi(category)
						_, ok := sourceIp.BlockCategories[cat]
						if ok {
							row.CategoryId = cat
							break
						}
					}
				}

				logEntryJson.CategoryId = row.CategoryId

				if !onCache {
					dl.Cache.Set(cacheKey, row, time.Minute)
					log.Infof("Save domain on cache")
				}

				_, ok := sourceIp.BlockCategories[row.CategoryId]
				if ok {
					m := new(dns.Msg).
						SetRcode(r, dns.RcodeSuccess).
						SetEdns0(4096, true)

					for _, question := range r.Question {
						if question.Qtype == dns.TypeA {
							// Criar um novo registro A com o IP de destino
							rr, err := dns.NewRR(fmt.Sprintf("%s A %s", question.Name, dl.BlockIp))
							if err != nil {
								log.Errorf("Erro ao criar registro A: %v", err)
								continue
							}
							// Adicionar o registro à seção de resposta
							m.Answer = append(m.Answer, rr)
						}
					}

					logEntryJson.AccessType = "BLOCK"
					dl.emitToUDPSocket(logEntryJson)

					w.WriteMsg(m)
					return dns.RcodeSuccess, nil
				}
			}
		}
	}

	// Registrar log no servidor
	rrw := dnstest.NewRecorder(w)
	rc, err := plugin.NextOrFailure(dl.Name(), dl.Next, ctx, rrw, r)
	if err != nil {
		clog.Warningf("Error processing DNS request: %v", err)
		return rc, err
	}

	// Preparar log para envio
	dl.emitToUDPSocket(logEntryJson)

	return rc, nil
}

// Name implements the Handler interface.
func (dl DNSLogger) Name() string { return "dnslogger" }
