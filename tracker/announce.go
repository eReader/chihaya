// Copyright 2014 The Chihaya Authors. All rights reserved.
// Use of this source code is governed by the BSD 2-Clause license,
// which can be found in the LICENSE file.

package tracker

import (
	"net"

	"github.com/chihaya/chihaya/stats"
	"github.com/chihaya/chihaya/tracker/models"
)

// HandleAnnounce encapsulates all of the logic of handling a BitTorrent
// client's Announce without being coupled to any transport protocol.
func (tkr *Tracker) HandleAnnounce(ann *models.Announce, w Writer) error {
	conn, err := tkr.Pool.Get()
	if err != nil {
		return err
	}

	defer conn.Close()

	if tkr.cfg.ClientWhitelistEnabled {
		if err = conn.FindClient(ann.ClientID()); err != nil {
			return err
		}
	}

	var user *models.User
	if tkr.cfg.PrivateEnabled {
		if user, err = conn.FindUser(ann.Passkey); err != nil {
			return err
		}
	}

	var torrent *models.Torrent
	torrent, err = conn.FindTorrent(ann.Infohash)
	switch {
	case !tkr.cfg.PrivateEnabled && err == models.ErrTorrentDNE:
		torrent = &models.Torrent{
			Infohash: ann.Infohash,
			Seeders:  models.PeerMap{},
			Leechers: models.PeerMap{},
		}

		err = conn.PutTorrent(torrent)
		if err != nil {
			return err
		}
		stats.RecordEvent(stats.NewTorrent)

	case err != nil:
		return err
	}

	var createdv4, createdv6, snatchedv4, snatchedv6 bool
	peer, peerv4, peerv6 := models.NewPeer(ann, user, torrent)

	if peerv4 != nil {
		createdv4, err = updateSwarm(conn, ann, peerv4, torrent)
		if err != nil {
			return err
		}
	}
	if peerv6 != nil {
		createdv6, err = updateSwarm(conn, ann, peerv6, torrent)
		if err != nil {
			return err
		}
	}

	if peerv4 != nil {
		snatchedv4, err = handleEvent(conn, ann, peerv4, user, torrent)
		if err != nil {
			return err
		}
	}
	if peerv6 != nil {
		snatchedv6, err = handleEvent(conn, ann, peerv6, user, torrent)
		if err != nil {
			return err
		}
	}

	created := createdv4 || createdv6
	snatched := snatchedv4 || snatchedv6

	if tkr.cfg.PrivateEnabled {
		delta := models.NewAnnounceDelta(ann, peer, user, torrent, created, snatched)
		err = tkr.backend.RecordAnnounce(delta)
		if err != nil {
			return err
		}
	} else if tkr.cfg.PurgeInactiveTorrents && torrent.PeerCount() == 0 {
		// Rather than deleting the torrent explicitly, let the tracker driver
		// ensure there are no race conditions.
		conn.PurgeInactiveTorrent(torrent.Infohash)
		stats.RecordEvent(stats.DeletedTorrent)
	}

	return w.WriteAnnounce(newAnnounceResponse(ann, peer, torrent))
}

// updateSwarm handles the changes to a torrent's swarm given an announce.
func updateSwarm(c Conn, ann *models.Announce, p *models.Peer, t *models.Torrent) (created bool, err error) {
	c.TouchTorrent(t.Infohash)

	switch {
	case t.InSeederPool(p):
		err = c.PutSeeder(t.Infohash, p)
		if err != nil {
			return
		}
		t.Seeders[p.Key()] = *p

	case t.InLeecherPool(p):
		err = c.PutLeecher(t.Infohash, p)
		if err != nil {
			return
		}
		t.Leechers[p.Key()] = *p

	default:
		if ann.Event != "" && ann.Event != "started" {
			err = models.ErrBadRequest
			return
		}

		if ann.Left == 0 {
			err = c.PutSeeder(t.Infohash, p)
			if err != nil {
				return
			}
			t.Seeders[p.Key()] = *p
			stats.RecordPeerEvent(stats.NewSeed, p.HasIPv6())

		} else {
			err = c.PutLeecher(t.Infohash, p)
			if err != nil {
				return
			}
			t.Leechers[p.Key()] = *p
			stats.RecordPeerEvent(stats.NewLeech, p.HasIPv6())
		}
		created = true
	}

	return
}

// handleEvent checks to see whether an announce has an event and if it does,
// properly handles that event.
func handleEvent(c Conn, ann *models.Announce, p *models.Peer, u *models.User, t *models.Torrent) (snatched bool, err error) {
	switch {
	case ann.Event == "stopped" || ann.Event == "paused":
		// updateSwarm checks if the peer is active on the torrent,
		// so one of these branches must be followed.
		if t.InSeederPool(p) {
			err = c.DeleteSeeder(t.Infohash, p)
			if err != nil {
				return
			}
			delete(t.Seeders, p.Key())
			stats.RecordPeerEvent(stats.DeletedSeed, p.HasIPv6())

		} else if t.InLeecherPool(p) {
			err = c.DeleteLeecher(t.Infohash, p)
			if err != nil {
				return
			}
			delete(t.Leechers, p.Key())
			stats.RecordPeerEvent(stats.DeletedLeech, p.HasIPv6())
		}

	case ann.Event == "completed":
		err = c.IncrementTorrentSnatches(t.Infohash)
		if err != nil {
			return
		}
		t.Snatches++

		if ann.Config.PrivateEnabled {
			err = c.IncrementUserSnatches(u.Passkey)
			if err != nil {
				return
			}
			u.Snatches++
		}

		if t.InLeecherPool(p) {
			err = leecherFinished(c, t, p)
		} else {
			err = models.ErrBadRequest
		}

		// If one of the dual-stacked peers is already a seeder, they have already
		// snatched.
		_, v4seed := t.Seeders[models.NewPeerKey(p.ID, false)]
		_, v6seed := t.Seeders[models.NewPeerKey(p.ID, true)]

		if !(v4seed || v6seed) {
			snatched = true
		}

	case t.InLeecherPool(p) && ann.Left == 0:
		// A leecher completed but the event was never received.
		err = leecherFinished(c, t, p)
	}

	return
}

// leecherFinished moves a peer from the leeching pool to the seeder pool.
func leecherFinished(c Conn, t *models.Torrent, p *models.Peer) error {
	if err := c.DeleteLeecher(t.Infohash, p); err != nil {
		return err
	}
	delete(t.Leechers, p.Key())

	if err := c.PutSeeder(t.Infohash, p); err != nil {
		return err
	}
	t.Seeders[p.Key()] = *p

	stats.RecordPeerEvent(stats.Completed, p.HasIPv6())
	return nil
}

func newAnnounceResponse(ann *models.Announce, announcer *models.Peer, t *models.Torrent) *models.AnnounceResponse {
	seedCount := len(t.Seeders)
	leechCount := len(t.Leechers)

	res := &models.AnnounceResponse{
		Complete:    seedCount,
		Incomplete:  leechCount,
		Interval:    ann.Config.Announce.Duration,
		MinInterval: ann.Config.MinAnnounce.Duration,
		Compact:     ann.Compact,
	}

	if ann.NumWant > 0 && ann.Event != "stopped" && ann.Event != "paused" {
		res.IPv4Peers, res.IPv6Peers = getPeers(ann, announcer, t, ann.NumWant)
	}

	return res
}

// getPeers returns lists IPv4 and IPv6 peers on a given torrent sized according
// to the wanted parameter.
func getPeers(ann *models.Announce, announcer *models.Peer, t *models.Torrent, wanted int) (ipv4s, ipv6s models.PeerList) {
	ipv4s, ipv6s = models.PeerList{}, models.PeerList{}

	if ann.Left == 0 {
		// If they're seeding, give them only leechers.
		return appendPeers(ipv4s, ipv6s, ann, announcer, t.Leechers, wanted)
	}

	// If they're leeching, prioritize giving them seeders.
	ipv4s, ipv6s = appendPeers(ipv4s, ipv6s, ann, announcer, t.Seeders, wanted)
	return appendPeers(ipv4s, ipv6s, ann, announcer, t.Leechers, wanted-len(ipv4s)-len(ipv6s))
}

// appendPeers implements the logic of adding peers to the IPv4 or IPv6 lists.
func appendPeers(ipv4s, ipv6s models.PeerList, ann *models.Announce, announcer *models.Peer, peers models.PeerMap, wanted int) (models.PeerList, models.PeerList) {
	if ann.Config.PreferredSubnet {
		return appendSubnetPeers(ipv4s, ipv6s, ann, announcer, peers, wanted)
	}

	count := 0

	for _, peer := range peers {
		if count >= wanted {
			break
		} else if peersEquivalent(&peer, announcer) {
			continue
		}

		if ann.HasIPv6() && peer.HasIPv6() {
			ipv6s = append(ipv6s, peer)
			count++
		} else if peer.HasIPv4() {
			ipv4s = append(ipv4s, peer)
			count++
		}
	}

	return ipv4s, ipv6s
}

// appendSubnetPeers is an alternative version of appendPeers used when the
// config variable PreferredSubnet is enabled.
func appendSubnetPeers(ipv4s, ipv6s models.PeerList, ann *models.Announce, announcer *models.Peer, peers models.PeerMap, wanted int) (models.PeerList, models.PeerList) {
	var subnetIPv4 net.IPNet
	var subnetIPv6 net.IPNet

	if ann.HasIPv4() {
		subnetIPv4 = net.IPNet{ann.IPv4, net.CIDRMask(ann.Config.PreferredIPv4Subnet, 32)}
	}

	if ann.HasIPv6() {
		subnetIPv6 = net.IPNet{ann.IPv6, net.CIDRMask(ann.Config.PreferredIPv6Subnet, 128)}
	}

	// Iterate over the peers twice: first add only peers in the same subnet and
	// if we still need more peers grab ones that haven't already been added.
	count := 0
	for _, checkInSubnet := range [2]bool{true, false} {
		for _, peer := range peers {
			if count >= wanted {
				break
			}

			inSubnet4 := peer.HasIPv4() && subnetIPv4.Contains(peer.IP)
			inSubnet6 := peer.HasIPv6() && subnetIPv6.Contains(peer.IP)

			if peersEquivalent(&peer, announcer) || checkInSubnet != (inSubnet4 || inSubnet6) {
				continue
			}

			if ann.HasIPv6() && peer.HasIPv6() {
				ipv6s = append(ipv6s, peer)
				count++
			} else if peer.HasIPv4() {
				ipv4s = append(ipv4s, peer)
				count++
			}
		}
	}

	return ipv4s, ipv6s
}

// peersEquivalent checks if two peers represent the same entity.
func peersEquivalent(a, b *models.Peer) bool {
	return a.ID == b.ID || a.UserID != 0 && a.UserID == b.UserID
}
