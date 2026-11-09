*** Settings ***
Documentation     Merged EC2 documents: VPC, two subnets, routing, SSH key, nginx VM, stop and dependency-ordered teardown. Mock only.
Library           AwsNginxLibrary.py
Suite Setup       Prepare AWS Nginx    ${BINARY}
Suite Teardown    Stop AWS Nginx Mock

*** Variables ***
${BINARY}    ${EXECDIR}/build/aws-nginx

*** Test Cases ***
Create And Teardown The Patched AWS Nginx Stack
    Create AWS Nginx
    Inspect AWS Nginx
    Destroy AWS Nginx
