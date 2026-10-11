variable "vm_name" {
  type    = string
  default = "tt-openstack-lab"
}

variable "memory_mib" {
  type    = number
  default = 4096
  validation {
    condition     = var.memory_mib >= 4096
    error_message = "DevStack requiere al menos 4096 MiB asignados a la VM."
  }
}

variable "vcpus" {
  type    = number
  default = 4
  validation {
    condition     = var.vcpus >= 4
    error_message = "Asignar al menos 4 vCPU."
  }
}

variable "disk_gib" {
  type    = number
  default = 55
  validation {
    condition     = var.disk_gib >= 50
    error_message = "Asignar al menos 50 GiB de disco."
  }
}

variable "ssh_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}
