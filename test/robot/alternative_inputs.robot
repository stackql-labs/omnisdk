*** Settings ***
Documentation     Alternative input requirements: Google bulkInsert accepts a template, explicit properties, or both.
Library           AlternativeInputsLibrary.py
Suite Setup       Prepare Alternative Inputs    ${BINARY}
Suite Teardown    Stop Alternative Inputs

*** Variables ***
${BINARY}    ${EXECDIR}/build/omnicli

*** Test Cases ***
Template Alone Satisfies The Alternative Requirement
    Run Bulk Insert Case    template

Explicit Properties Alone Satisfy The Alternative Requirement
    Run Bulk Insert Case    properties

Template And Property Overrides Are Both Permitted
    Run Bulk Insert Case    both

Neither Alternative Is Rejected Before Mutation
    Run Bulk Insert Case    neither

An Alternative Does Not Waive Required Project
    Run Bulk Insert Case    missing-project

An Alternative Does Not Waive Required Count
    Run Bulk Insert Case    missing-count
