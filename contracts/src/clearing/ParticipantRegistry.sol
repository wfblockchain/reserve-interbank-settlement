// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControl } from "@openzeppelin/contracts/access/AccessControl.sol";
import { IParticipantRegistry } from "./SettlementToken.sol";

/**
 * @title ParticipantRegistry
 * @notice Admission lists, one per policy: who may hold the settlement token
 *         (members), and each bank's customers who may hold its deposit token.
 *
 * @dev The settlement token, the deposit tokens and the intraday pool read
 *      admission through {IParticipantRegistry}, so this contract is the
 *      reference one and a production registry drops in unchanged.
 *
 *      Each policy has its own admin role: the operator governs the member
 *      policy, and each bank governs its own customer policy. A bank cannot
 *      admit a customer into another bank's list, or a member into the network.
 */
contract ParticipantRegistry is AccessControl, IParticipantRegistry {

    mapping(uint64 policyId => mapping(address account => bool)) private _authorized;

    event Admitted(uint64 indexed policyId, address indexed account);
    event Revoked(uint64 indexed policyId, address indexed account);

    error NotPolicyAdmin(uint64 policyId, address caller);

    constructor(address admin) {
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
    }

    /// @notice The role that governs a policy's list.
    function policyAdminRole(uint64 policyId) public pure returns (bytes32) {
        return keccak256(abi.encode("POLICY_ADMIN", policyId));
    }

    function admit(uint64 policyId, address account) external {
        _onlyPolicyAdmin(policyId);
        _authorized[policyId][account] = true;
        emit Admitted(policyId, account);
    }

    function revoke(uint64 policyId, address account) external {
        _onlyPolicyAdmin(policyId);
        _authorized[policyId][account] = false;
        emit Revoked(policyId, account);
    }

    function isAuthorized(uint64 policyId, address account) external view returns (bool) {
        return _authorized[policyId][account];
    }

    function _onlyPolicyAdmin(uint64 policyId) internal view {
        if (!hasRole(policyAdminRole(policyId), msg.sender)) {
            revert NotPolicyAdmin(policyId, msg.sender);
        }
    }

}
