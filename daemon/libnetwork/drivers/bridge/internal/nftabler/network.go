//go:build linux

package nftabler

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"go.opentelemetry.io/otel"
)

type network struct {
	config   firewaller.NetworkConfig
	fw       *Nftabler
	remover4 *nftables.Modifier
	remover6 *nftables.Modifier
}

func (nft *Nftabler) NewNetwork(ctx context.Context, nc firewaller.NetworkConfig) (_ firewaller.Network, retErr error) {
	n := &network{
		fw:     nft,
		config: nc,
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{"bridge": n.config.IfName}))

	if nft.cleaner != nil {
		nft.cleaner.DelNetwork(ctx, nc)
	}

	if n.fw.config.IPv4 {
		remover, err := n.configure(ctx, nft.table4, n.config.Config4)
		if err != nil {
			return nil, err
		}
		n.remover4 = remover
	}
	if n.fw.config.IPv6 {
		remover, err := n.configure(ctx, nft.table6, n.config.Config6)
		if err != nil {
			return nil, err
		}
		n.remover6 = remover
	}
	return n, nil
}

func (n *network) configure(ctx context.Context, table nftables.Table, conf firewaller.NetworkConfigFam) (*nftables.Modifier, error) {
	if !conf.Prefix.IsValid() {
		return nil, nil
	}
	tm := nftables.Modifier{}
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".newNetwork."+string(table.Family()))
	defer span.End()

	fwdInChain := chainFilterFwdIn(n.config.IfName)
	fwdOutChain := chainFilterFwdOut(n.config.IfName)
	natPostRtInChain := chainNatPostRtIn(n.config.IfName)
	natPostRtOutChain := chainNatPostRtOut(n.config.IfName)

	// Filter chain

	tm.Create(nftables.Chain{Name: fwdInChain})
	tm.Create(nftables.Chain{Name: fwdOutChain})

	tm.Create(nftables.VMapElement{
		VmapName: filtFwdInVMap,
		Key:      n.config.IfName,
		Verdict:  "jump " + fwdInChain,
	})
	tm.Create(nftables.VMapElement{
		VmapName: filtFwdOutVMap,
		Key:      n.config.IfName,
		Verdict:  "jump " + fwdOutChain,
	})

	// NAT chain

	tm.Create(nftables.Chain{Name: natPostRtInChain})
	tm.Create(nftables.VMapElement{
		VmapName: natPostroutingInVMap,
		Key:      n.config.IfName,
		Verdict:  "jump " + natPostRtInChain,
	})

	tm.Create(nftables.Chain{Name: chainNatPostRtOut(n.config.IfName)})
	tm.Create(nftables.VMapElement{
		VmapName: natPostroutingOutVMap,
		Key:      n.config.IfName,
		Verdict:  "jump " + chainNatPostRtOut(n.config.IfName),
	})

	// Conntrack

	tm.Create(nftables.Rule{
		Chain: chainFilterFwdIn(n.config.IfName),
		Group: initialRuleGroup,
		Rule:  []string{"ct state established,related counter accept"},
	})
	tm.Create(nftables.Rule{
		Chain: chainFilterFwdOut(n.config.IfName),
		Group: initialRuleGroup,
		Rule:  []string{"ct state established,related counter accept"},
	})

	iccVerdict := "accept"
	if !n.config.ICC {
		iccVerdict = "drop"
	}

	if n.config.Internal {
		// Drop anything that's not from this network.
		tm.Create(nftables.Rule{
			Chain: fwdInChain,
			Group: initialRuleGroup,
			Rule:  []string{`iifname != `, n.config.IfName, `counter drop comment "INTERNAL NETWORK INGRESS"`},
		})
		tm.Create(nftables.Rule{
			Chain: fwdOutChain,
			Group: initialRuleGroup,
			Rule:  []string{`oifname != `, n.config.IfName, `counter drop comment "INTERNAL NETWORK EGRESS"`},
		})

		// Accept or drop Inter-Container Communication.
		tm.Create(nftables.Rule{
			Chain: fwdInChain,
			Group: fwdInICCRuleGroup,
			Rule:  []string{"counter", iccVerdict, "comment ICC"},
		})
	} else {
		// AcceptFwMark
		if n.config.AcceptFwMark != "" {
			fwm, err := nftFwMark(n.config.AcceptFwMark)
			if err != nil {
				return nil, fmt.Errorf("adding fwmark %q for %q: %w", n.config.AcceptFwMark, n.config.IfName, err)
			}
			tm.Create(nftables.Rule{
				Chain: fwdInChain,
				Group: fwdInAcceptFwMarkRuleGroup,
				Rule:  []string{`meta mark `, fwm, ` counter accept comment "ALLOW FW MARK"`},
			})
		}

		// Inter-Container Communication
		tm.Create(nftables.Rule{
			Chain: fwdInChain,
			Group: fwdInICCRuleGroup,
			Rule:  []string{"iifname ==", n.config.IfName, "counter", iccVerdict, "comment ICC"},
		})

		// Outgoing traffic
		tm.Create(nftables.Rule{
			Chain: fwdOutChain,
			Group: initialRuleGroup,
			Rule:  []string{"counter accept comment OUTGOING"},
		})

		// Incoming traffic
		if conf.Unprotected {
			tm.Create(nftables.Rule{
				Chain: fwdInChain,
				Group: fwdInFinalRuleGroup,
				Rule:  []string{`counter accept comment "UNPROTECTED"`},
			})
		} else {
			tm.Create(nftables.Rule{
				Chain: fwdInChain,
				Group: fwdInFinalRuleGroup,
				Rule:  []string{`counter drop comment "UNPUBLISHED PORT DROP"`},
			})
		}

		// ICMP
		if conf.Routed {
			rule := "ip protocol icmp"
			if table.Family() == nftables.IPv6 {
				rule = "meta l4proto ipv6-icmp"
			}
			tm.Create(nftables.Rule{
				Chain: fwdInChain,
				Group: initialRuleGroup,
				Rule:  []string{rule, "counter accept comment ICMP"},
			})
		}

		// Masquerade / SNAT - masquerade picks a source IP address based on next-hop, SNAT uses conf.HostIP.
		natPostroutingVerdict := "masquerade"
		natPostroutingComment := "MASQUERADE"
		if conf.HostIP.IsValid() {
			natPostroutingVerdict = "snat to " + conf.HostIP.Unmap().String()
			natPostroutingComment = "SNAT"
		}
		if n.config.Masquerade && !conf.Routed {
			tm.Create(nftables.Rule{
				Chain: natPostRtOutChain,
				Group: initialRuleGroup,
				Rule: []string{
					"oifname !=", n.config.IfName, string(table.Family()), "saddr", conf.Prefix.String(), "counter",
					natPostroutingVerdict, "comment", natPostroutingComment,
				},
			})
		}
		if n.fw.config.Hairpin {
			tm.Create(nftables.Rule{
				Chain: natPostRtInChain,
				Group: initialRuleGroup,
				Rule: []string{
					`fib saddr type local counter`, natPostroutingVerdict, `comment "` + natPostroutingComment + ` FROM HOST"`,
				},
			})
		}
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
		"bridge": n.config.IfName,
		"family": table.Family(),
	}))
	if err := table.Apply(ctx, tm); err != nil {
		return nil, fmt.Errorf("adding rules for bridge %s: %w", n.config.IfName, err)
	}
	undoer := tm.Reverse()
	return &undoer, nil
}

func (n *network) ReapplyNetworkLevelRules(ctx context.Context) error {
	// A firewalld reload doesn't delete nftables rules, this function is not needed.
	log.G(ctx).Warn("ReapplyNetworkLevelRules is not implemented for nftables")
	return nil
}

func (n *network) DelNetworkLevelRules(ctx context.Context) error {
	remove := func(t nftables.Table, remover *nftables.Modifier) {
		if remover != nil {
			ctx := log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{"bridge": n.config.IfName}))
			if err := t.Apply(ctx, *remover); err != nil {
				log.G(ctx).WithError(err).Warn("Failed to remove network rules for network")
			}
		}
	}
	if n.remover4 != nil {
		remove(n.fw.table4, n.remover4)
		n.remover4 = nil
	}
	if n.remover6 != nil {
		remove(n.fw.table6, n.remover6)
		n.remover6 = nil
	}
	return nil
}

func chainFilterFwdIn(ifName string) string {
	return "filter-forward-in__" + ifName
}

func chainFilterFwdOut(ifName string) string {
	return "filter-forward-out__" + ifName
}

func chainNatPostRtOut(ifName string) string {
	return "nat-postrouting-out__" + ifName
}

func chainNatPostRtIn(ifName string) string {
	return "nat-postrouting-in__" + ifName
}

// nftFwMark takes a string representing a firewall mark with an optional
// "/mask", parses the mark and mask, and returns an nftables expression
// representing the same mask/mark. Numbers are converted to decimal, because
// strings.ParseUint accepts more integer formats than nft.
func nftFwMark(val string) (string, error) {
	markStr, maskStr, haveMask := strings.Cut(val, "/")
	mark, err := strconv.ParseUint(markStr, 0, 32)
	if err != nil {
		return "", fmt.Errorf("invalid firewall mark %q: %w", val, err)
	}
	if haveMask {
		mask, err := strconv.ParseUint(maskStr, 0, 32)
		if err != nil {
			return "", fmt.Errorf("invalid firewall mask %q: %w", val, err)
		}
		return fmt.Sprintf("and %d == %d", mask, mark), nil
	}
	return strconv.FormatUint(mark, 10), nil
}
