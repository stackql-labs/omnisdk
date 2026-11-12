# IaC examples

- [Update routing](update-routing/README.md): runnable local create, no-op, update,
  rejected change and teardown, with a self-contained provider registry.
- [AWS VPC/subnet](aws-vpc-subnet/README.md): explicit network-resource input for the
  existing AWS IaC path, with apply and teardown commands.

Generated ledgers and transcripts belong under `cicd/out/`, outside these source examples.

- [AWS nginx lifecycle](aws-nginx/README.md): merge the existing EC2 document, create VPC/subnets,
  internet routing, SSH key, security group and a custom nginx VM; stop and tear down using the ledger.
