// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { ParticipantRegistry } from "../src/clearing/ParticipantRegistry.sol";
import { Test } from "forge-std/Test.sol";

contract ParticipantRegistryTest is Test {

    uint64 constant MEMBERS = 1;
    uint64 constant BANK_A_CUSTOMERS = 2;

    ParticipantRegistry registry;
    address operator = makeAddr("operator");
    address bankA = makeAddr("bankA");
    address alice = makeAddr("alice");

    function setUp() public {
        registry = new ParticipantRegistry(address(this));
        registry.grantRole(registry.policyAdminRole(MEMBERS), operator);
        registry.grantRole(registry.policyAdminRole(BANK_A_CUSTOMERS), bankA);
    }

    function test_PolicyAdminAdmitsAndRevokes() public {
        vm.prank(operator);
        registry.admit(MEMBERS, bankA);
        assertTrue(registry.isAuthorized(MEMBERS, bankA));

        vm.prank(operator);
        registry.revoke(MEMBERS, bankA);
        assertFalse(registry.isAuthorized(MEMBERS, bankA));
    }

    function test_ABankCannotAdmitIntoAnotherList() public {
        // Bank A governs its own customers, not the network's members.
        vm.prank(bankA);
        vm.expectRevert(
            abi.encodeWithSelector(ParticipantRegistry.NotPolicyAdmin.selector, MEMBERS, bankA)
        );
        registry.admit(MEMBERS, bankA);

        vm.prank(bankA);
        registry.admit(BANK_A_CUSTOMERS, alice);
        assertTrue(registry.isAuthorized(BANK_A_CUSTOMERS, alice));
        assertFalse(registry.isAuthorized(MEMBERS, alice));
    }

    function test_TheOperatorCannotAdmitABanksCustomer() public {
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                ParticipantRegistry.NotPolicyAdmin.selector, BANK_A_CUSTOMERS, operator
            )
        );
        registry.admit(BANK_A_CUSTOMERS, alice);
    }

}
