*** Settings ***
Documentation     Explicit accounts of the same provider through the real CLI; no cloud calls.
Library           ProviderAliasLibrary.py
Suite Setup       Prepare Provider Aliases    ${BINARY}
Suite Teardown    Stop Provider Aliases

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Aliases Select Distinct Credentials And Regions
    Aliases Use Explicit Credentials And Regions

Missing Alias Auth Cannot Borrow Global Credentials
    Missing Alias Auth Does Not Fall Back

Aliases Sharing A Tenancy Cannot Disagree On Its Limit
    Same Tenancy Conflicting Limits Are Rejected

Unresolved Secret References Fail Before Effects
    Unresolved Secret Binding Is Rejected
