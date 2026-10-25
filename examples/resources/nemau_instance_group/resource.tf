resource "nemau_instance_group" "web" {
  name       = "web-pool"
  network_id = nemau_network.backend.id
  subnet_id  = nemau_subnet.app.id
  min_size   = 2
  max_size   = 6

  # Omit desired_capacity: the autoscaler decides it. Set it to move it once.
  security_groups = [nemau_security_group.web.id]

  launch_template {
    image_id  = data.nemau_images.debian.images[0].id
    flavor_id = "small"
    key_name  = "deploy"
    # cloud-init document of each member; never returned by the API.
    user_data = file("${path.module}/cloud-init.yaml")
  }

  load_balancer_pool {
    pool_id       = nemau_pool.web.id
    protocol_port = 80
  }

  scaling_policy {
    metric         = "cpu_percent"
    target         = 60
    window_seconds = 300
  }

  scheduled_action {
    cron     = "0 8 * * 1-5"
    min_size = 4
  }

  scheduled_action {
    cron     = "0 20 * * 1-5"
    min_size = 2
  }
}

# The group's members, once the group exists.
output "web_members" {
  value = nemau_instance_group.web.instance_count
}
