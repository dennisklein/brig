---
title: "Move a VM onto a newer base image"
concepts: [base-image, data-disk, root-disk]
commands:
  - "brig image build"
  - "brig upgrade dev"
show:
  href: "/images"
  action: image-build
doc: "how-to/upgrade.md#vm-and-openshell"
---
1. Build a base image if Images has none newer than the VM's.
2. Open the VM and press Upgrade. The data disk stays; the root disk is
   replaced.
3. If the gateway does not answer on the new image, brig rolls back.
