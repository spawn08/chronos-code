<?php

namespace App;

use App\Contracts\Handler;

function helper(): string {
    return "ok";
}

class UserService {
    public function findUser(int $id): ?User {
        return null;
    }
}
