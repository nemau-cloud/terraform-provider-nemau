# A rule imports as <security_group_id>:<rule_id>, because the API serves a
# rule only within its group. `nemau network security-group rules <group-id>`
# lists the rule ids.
terraform import nemau_security_group_rule.https 9b5d3e1f-2a7c-4f68-9d41-6e8a0c3b7f25:5c1a8e3d-7b4f-4a92-8e60-2d9f1b6c4a57
