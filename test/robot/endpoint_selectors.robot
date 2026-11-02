*** Settings ***
Documentation     Opt-in token endpoint selection with explicitly bound API scope; no cloud calls.
Library           EndpointSelectorLibrary.py
Suite Setup       Prepare Endpoint Selectors    ${BINARY}
Suite Teardown    Stop Endpoint Selectors
Test Template     Check Endpoint Selector

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***                                   SCENARIO
Unconfigured Selector Uses Its Declared Default      default
Selector Moves Token And API To The Same Host        selector
Direct Override Wins Over Selector                   direct
Runtime Token Url Bypasses Document Variables        runtime
Unknown Selector Fails Before Any Request            unknown
Server Selector Remains Explicit Host Metadata       unbound
