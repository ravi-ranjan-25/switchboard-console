locals {
  # hostname -> zone name
  tunnel_hosts = {
    "example.com"            = "example.com"
    "*.example.com"          = "example.com"
    "client.example.org"     = "client.example.org"
    "www.client.example.org" = "client.example.org"
  }
}

data "cloudflare_zone" "z" {
  for_each = toset(var.zones)
  filter = {
    name = each.value
  }
}
