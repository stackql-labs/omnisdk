*** Settings ***
Documentation     Identity classes through real CLI processes and an isolated HTTP API. No cloud credentials.
Library           IdentityLibrary.py
Test Teardown     Stop Identity Lifecycle

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Provider Generated Eager Key Lifecycle
    Run Identity Lifecycle    eager

Await With Eager Key Lifecycle
    Run Identity Lifecycle    await-eager

Await With Late Key Lifecycle
    Run Identity Lifecycle    await-late

Caller Supplied Name Lifecycle
    Run Identity Lifecycle    caller-name

Await Failure Never Releases Dependents Or Deletes Unknown Resources
    Start Identity Lifecycle    ${BINARY}    await-error
    Await Failure Blocks Dependents

Await Timeout Never Releases Dependents Or Deletes Unknown Resources
    Start Identity Lifecycle    ${BINARY}    await-timeout
    Await Failure Blocks Dependents

Live Google Early Identity Documents Work Through The CLI
    Run Live Google Document Lifecycle    ${BINARY}    await-eager

Live Google Late Identity Documents Work Through The CLI
    Run Live Google Document Lifecycle    ${BINARY}    await-late

Live Google Caller Name Documents Work Through The CLI
    Run Live Google Document Lifecycle    ${BINARY}    caller-name

Selected Deletion Validates All Keys Before Effects And Orders Dependents First
    Selected Deletion Is Atomic And Ordered    ${BINARY}

Caller Named Collision Never Adopts Or Deletes The Existing Object
    Existing Caller Named Object Is Not Adopted    ${BINARY}

*** Keywords ***
Run Identity Lifecycle
    [Arguments]    ${mode}
    Start Identity Lifecycle    ${BINARY}    ${mode}
    Create And Check Identity
    Update Repeatedly    3
    Prune Child And Continue
    Generate Local Key And Use Public Digest
    Destroy And Check Empty
