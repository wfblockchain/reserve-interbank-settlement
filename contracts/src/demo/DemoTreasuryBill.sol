// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import { AccessControl } from "@openzeppelin/contracts/access/AccessControl.sol";
import { ERC20 } from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/**
 * @title DemoTreasuryBill
 * @notice DEMO ONLY. Stands in for a tokenized Treasury bill that banks post as
 *         collateral to the intraday liquidity pool. A real deployment registers
 *         an existing tokenized HQLA instrument with the pool instead; nothing in
 *         the clearing contracts depends on this one.
 */
contract DemoTreasuryBill is ERC20, AccessControl {

    bytes32 public constant ISSUER_ROLE = keccak256("ISSUER_ROLE");

    constructor(address admin) ERC20("Demo Treasury Bill", "dTBILL") {
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
        _grantRole(ISSUER_ROLE, admin);
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external onlyRole(ISSUER_ROLE) {
        _mint(to, amount);
    }

}
