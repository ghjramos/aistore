---
layout: post
title: JOIN CLUSTER
permalink: /docs/join-cluster
redirect_from:
 - /join_cluster.md/
 - /docs/join_cluster.md/
---

**Note**: for the most recent updates on the topic of cluster membership and node lifecycle, please also check:

* [Node lifecycle: maintenance mode, rebalance/rebuild, shutdown, decommission](/docs/lifecycle_node.md)

Also, see related:

* [Leaving aistore cluster](leave_cluster.md)
* [Global rebalance](rebalance.md)
* [CLI: `ais cluster` command](/docs/cli/cluster.md)
* [Scripted integration tests](https://github.com/NVIDIA/aistore/tree/main/ais/test/scripts)

## Joining a Cluster: _discovery_ URL, and more

First, some basic facts. AIStore clusters can be deployed with an arbitrary number of AIStore proxies. Each proxy/gateway implements RESTful API and provides full access to objects stored in the cluster. Each proxy collaborates with all other proxies to perform majority-voted HA failovers (section [Highly Available Control Plane](ha.md).

There are some subtle differences between proxies, though. Two of them can be designated via [AIStore configuration](/deploy/dev/local/aisnode_config.sh)) as an *original* and a *discovery*. The *original* (located at the configurable `original_url`) is expected to point to the primary at the cluster initial deployment time.

Later on, when and if an HA event triggers automated failover, the role of the primary will be automatically assumed by a different proxy/gateway, with the corresponding cluster map (Smap) update getting synchronized across all running nodes.

A new node, however, could potentially experience a problem when trying to join an already deployed and running cluster - simply because its configuration may still be referring to the old primary. The `discovery_url` (see [AIStore configuration](/deploy/dev/local/aisnode_config.sh)) is precisely intended to address this scenario.

Here's how a new node joins a running AIStore cluster:

- first, there's the primary proxy/gateway referenced by the current cluster map (Smap) and/or - during the cluster deployment time - by the configured `primary_url` (see [AIStore configuration](/deploy/dev/local/aisnode_config.sh))

- if joining via the `primary_url` fails, then the new node goes ahead and tries the alternatives:
  - `discovery_url`
  - `original_url`
- but only if those are defined and different from the previously tried.
