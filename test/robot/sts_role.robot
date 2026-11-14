*** Settings ***
Documentation     Explicit AWS AssumeRole through the real CLI and a discrete Flask protocol fixture.
Library           STSRoleLibrary.py
Test Setup        Prepare STS    ${BINARY}
Test Teardown     Stop STS

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Concurrent Queries Share AssumeRole And Sign With The Returned Session
    Concurrent Queries Share Assume Role

Expired Role Credentials Refresh Before The Next Request
    Expired Role Credentials Refresh Before Next Request

Rejected AssumeRole Never Dispatches A Downstream Request
    Rejected Assume Role Never Dispatches Downstream

Expired STS Credentials Are Never Used Downstream
    Expired STS Response Never Dispatches Downstream
