# A cluster imports by its name. Its first node pool, the one created with
# it, becomes default_node_pool. The import reads network_id, subnet_id and the
# CIDRs from the cluster: the configuration must name the same values.
terraform import nemau_kubernetes_cluster.prod prod
