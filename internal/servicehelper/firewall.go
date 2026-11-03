package servicehelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func protectRCON(ctx context.Context, r runner, i model.Instance) error {
	table := "nec_" + i.ID
	args := []string{"--numeric", "--json", "list", "table", "inet", table}
	data, err := r.run(ctx, nft, nil, args...)
	if err != nil {
		batch := fmt.Sprintf("add table inet %s\nadd chain inet %s rcon { type filter hook input priority -150; policy accept; }\nadd rule inet %s rcon iifname != \"lo\" tcp dport %d drop\n", table, table, table, i.RCONPort)
		if _, err := r.run(ctx, nft, []byte(batch), "--file", "-"); err != nil {
			return errors.New("cannot install RCON ingress protection; instance was not started")
		}
		data, err = r.run(ctx, nft, nil, args...)
	}
	if err != nil || !validFirewall(data, table, i.RCONPort) {
		return errors.New("RCON ingress protection verification failed; instance was not started")
	}
	return nil
}

func validFirewall(data []byte, table string, port int) bool {
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	wantJSON := fmt.Sprintf(`[{"match":{"op":"!=","left":{"meta":{"key":"iifname"}},"right":"lo"}},{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":%d}},{"drop":null}]`, port)
	var want any
	_ = json.Unmarshal([]byte(wantJSON), &want)
	counts := map[string]int{}
	for _, entry := range doc.NFTables {
		if len(entry) != 1 {
			return false
		}
		for kind, raw := range entry {
			if kind == "metainfo" {
				continue
			}
			counts[kind]++
			var v map[string]any
			if json.Unmarshal(raw, &v) != nil || v["family"] != "inet" {
				return false
			}
			switch kind {
			case "table":
				if v["name"] != table || v["flags"] != nil {
					return false
				}
			case "chain":
				if v["table"] != table || v["name"] != "rcon" || v["type"] != "filter" || v["hook"] != "input" || v["prio"] != float64(-150) || v["policy"] != "accept" {
					return false
				}
			case "rule":
				if v["table"] != table || v["chain"] != "rcon" || !reflect.DeepEqual(v["expr"], want) {
					return false
				}
			default:
				return false
			}
		}
	}
	return counts["table"] == 1 && counts["chain"] == 1 && counts["rule"] == 1
}
