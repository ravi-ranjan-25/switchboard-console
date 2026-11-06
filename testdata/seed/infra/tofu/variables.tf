variable "zones" {
  type    = list(string)
  default = ["example.com", "client.example.org"]
}

variable "other" {
  type    = string
  default = "unrelated"
}
