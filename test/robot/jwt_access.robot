*** Settings ***
Documentation     Opt-in JWT reader policy through the compiled CLI; claims cannot override directory groups.
Library           JWTAccessLibrary.py
Suite Setup       Prepare JWT Access    ${BINARY}
Suite Teardown    Stop JWT Access

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Ed25519 Reader Can Select
    Reader Select Succeeds    EdDSA

RSA Reader Can Select
    Reader Select Succeeds    RS256

Reader Cannot Insert
    Reader Mutation Is Denied    insert

Reader Cannot Update
    Reader Mutation Is Denied    update

Reader Cannot Delete
    Reader Mutation Is Denied    delete

Reader Cannot Execute Actions
    Reader Mutation Is Denied    exec

Reader Cannot Apply Or Destroy IaC Or Touch Its Ledger
    Reader IaC Is Denied

Directory Groups Override Token Group Claims
    JWT Groups Come From Directory

Explicitly Authorized Operator Can Mutate
    Operator Mutation Succeeds

Directory And Policy Changes Fail Closed On A New Session
    Directory And Policy Fail Closed

Invalid JWT Never Reaches The Provider
    [Template]    JWT Variant Is Denied
    missing
    tampered
    wrong-issuer
    wrong-audience
    expired
    future-nbf
    future-iat
    no-expiry
    no-subject
    unknown-subject
    unknown-key
    none
    hmac-confusion
