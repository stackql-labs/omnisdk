*** Settings ***
Documentation     Merged EC2 documents: VPC, two subnets, routing, SSH key, nginx VM, stop and dependency-ordered teardown. Mock only.
Library           AwsNginxLibrary.py
Test Setup       Prepare AWS Nginx    ${BINARY}
Test Teardown    Stop AWS Nginx Mock

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Create And Teardown The Patched AWS Nginx Stack
    Create AWS Nginx
    Inspect AWS Nginx
    Destroy AWS Nginx

Lost Create Response Retains Ownership And Blocks Blind Recovery
    Lose Create Response And Verify Ledger

Interrupted Readiness Retains Poll Handle And Write Lock
    Interrupt Readiness And Verify Recovery

Lost Delete Response Retains Identity And Blocks Blind Retry
    Lose Delete Response And Verify Ledger
